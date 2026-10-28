package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Forwarder delivers a stored report somewhere else.
type Forwarder interface {
	Name() string
	Forward(ctx context.Context, rec *Record, files FileOpener) (url string, err error)
}

// FileOpener reads a report's stored attachment.
type FileOpener func(f StoredFile) ([]byte, error)

// PermanentError marks a failure that retrying won't fix (bad credentials, 4xx).
type PermanentError struct{ Err error }

func (e PermanentError) Error() string { return e.Err.Error() }
func (e PermanentError) Unwrap() error { return e.Err }

var httpClient = &http.Client{Timeout: 60 * time.Second}

func doHTTP(ctx context.Context, req *http.Request) ([]byte, error) {
	req = req.WithContext(ctx)
	req.Header.Set("User-Agent", "snag-server/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(body)), 300))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && resp.StatusCode != 425 && resp.StatusCode != 429 {
		if !(resp.StatusCode == 403 && strings.Contains(strings.ToLower(string(body)), "rate limit")) {
			return nil, PermanentError{err}
		}
	}
	return nil, err
}

// BuildForwarders creates the forwarders a project enables.
func BuildForwarders(p *Project, cfg Config, reportURL func(id string) string) []Forwarder {
	var out []Forwarder
	f := p.Forwarding
	if f.GitHub != nil && f.GitHub.Repo != "" {
		owner, repo, _ := strings.Cut(f.GitHub.Repo, "/")
		token := f.GitHub.Token
		if token == "" {
			token = cfg.GitHubToken
		}
		api := cfg.GitHubAPIURL
		if api == "" {
			api = "https://api.github.com"
		}
		out = append(out, &GitHubForwarder{cfg: GitHubConfig{
			Token: token, Owner: owner, Repo: repo, Labels: f.GitHub.Labels,
			AttachmentBranch: f.GitHub.AttachmentBranch, APIURL: api,
		}, reportURL: reportURL})
	}
	if f.Discord != "" {
		out = append(out, &DiscordForwarder{webhook: f.Discord, reportURL: reportURL})
	}
	if f.Slack != "" {
		out = append(out, &SlackForwarder{webhook: f.Slack, reportURL: reportURL})
	}
	if f.Webhook != nil && f.Webhook.URL != "" {
		out = append(out, &WebhookForwarder{cfg: *f.Webhook})
	}
	if len(f.Email) > 0 && cfg.SMTP != nil {
		smtp := *cfg.SMTP
		smtp.To = f.Email
		out = append(out, &EmailForwarder{cfg: smtp, reportURL: reportURL})
	}
	return out
}

// MARK: Dispatcher

// Dispatcher runs forwards in the background with retries. State lives in each record, so
// pending forwards resume after a restart.
type Dispatcher struct {
	store     *Store
	projects  *ProjectStore
	cfg       Config
	reportURL func(string) string
	log       *slog.Logger
	queue     chan string
	mu        sync.Mutex
	inFlight  map[string]bool
	// Backoff is the delay after the nth failure.
	Backoff     func(attempt int) time.Duration
	MaxAttempts int
	// Build makes a project's forwarders; tests replace it.
	Build func(p *Project) []Forwarder
	// OnDelivered runs after a successful forward.
	OnDelivered func(rec *Record, forwarder, url string)
	// OnAttempt runs after every delivery attempt.
	OnAttempt func(forwarder string, err error)
}

func NewDispatcher(store *Store, projects *ProjectStore, cfg Config, reportURL func(string) string, logger *slog.Logger) *Dispatcher {
	d := &Dispatcher{
		store:       store,
		projects:    projects,
		cfg:         cfg,
		reportURL:   reportURL,
		log:         logger,
		queue:       make(chan string, 1024),
		inFlight:    map[string]bool{},
		MaxAttempts: 20,
		Backoff: func(attempt int) time.Duration {
			d := 15 * time.Second << min(attempt-1, 10)
			if d > 2*time.Hour {
				d = 2 * time.Hour
			}
			return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
		},
	}
	d.Build = func(p *Project) []Forwarder { return BuildForwarders(p, d.cfg, d.reportURL) }
	return d
}

func (d *Dispatcher) forwarders(slug string) map[string]Forwarder {
	out := map[string]Forwarder{}
	if p, ok := d.projects.Get(slug); ok {
		for _, f := range d.Build(p) {
			out[f.Name()] = f
		}
	}
	return out
}

// Names lists the forwarders a project has, sorted.
func (d *Dispatcher) Names(slug string) []string {
	var names []string
	for name := range d.forwarders(slug) {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// InitialState is the forwarding state for a report that opens a new issue.
func (d *Dispatcher) InitialState(slug string) map[string]*ForwardState {
	names := d.Names(slug)
	if len(names) == 0 {
		return nil
	}
	state := map[string]*ForwardState{}
	for _, name := range names {
		state[name] = &ForwardState{}
	}
	return state
}

func (d *Dispatcher) Enqueue(id string) {
	select {
	case d.queue <- id:
	default:
		// Queue full; the periodic sweep picks it up.
	}
}

// Retry clears backoff for one report's undelivered forwards (adding any the project gained
// since) and queues it.
func (d *Dispatcher) Retry(id string) error {
	rec, err := d.store.Get(id)
	if err != nil {
		return err
	}
	names := d.Names(rec.Project)
	_, err = d.store.Update(id, func(rec *Record) {
		if rec.Forwarding == nil {
			rec.Forwarding = map[string]*ForwardState{}
		}
		for _, name := range names {
			st, ok := rec.Forwarding[name]
			if !ok {
				st = &ForwardState{}
				rec.Forwarding[name] = st
			}
			if !st.Delivered {
				st.GaveUp = false
				st.NextAttempt = time.Time{}
				st.Attempts = 0
				st.Error = ""
			}
		}
	})
	if err == nil {
		d.Enqueue(id)
	}
	return err
}

// Run processes the queue until ctx ends, sweeping for due retries every interval.
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) {
	for _, rec := range d.store.Pending() {
		d.Enqueue(rec.ID)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-d.queue:
			d.Process(ctx, id)
		case <-ticker.C:
			for _, rec := range d.store.Pending() {
				d.Process(ctx, rec.ID)
			}
		}
	}
}

// Process attempts every due forward for one report.
func (d *Dispatcher) Process(ctx context.Context, id string) {
	d.mu.Lock()
	if d.inFlight[id] {
		d.mu.Unlock()
		return
	}
	d.inFlight[id] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.inFlight, id)
		d.mu.Unlock()
	}()

	rec, err := d.store.Get(id)
	if err != nil {
		return
	}
	now := time.Now()
	opener := func(f StoredFile) ([]byte, error) {
		path, _, err := d.store.FilePath(rec.ID, f.Name)
		if err != nil {
			return nil, err
		}
		return os.ReadFile(path)
	}
	forwarders := d.forwarders(rec.Project)
	names := make([]string, 0, len(rec.Forwarding))
	for name := range rec.Forwarding {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		st := rec.Forwarding[name]
		if st == nil || st.Delivered || st.GaveUp || now.Before(st.NextAttempt) {
			continue
		}
		forwarder, ok := forwarders[name]
		if !ok {
			_, _ = d.store.Update(id, func(r *Record) {
				if s := r.Forwarding[name]; s != nil {
					s.GaveUp, s.Error = true, "no longer configured for this project"
				}
			})
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		url, ferr := forwarder.Forward(fctx, rec, opener)
		cancel()
		if ctx.Err() != nil {
			return
		}
		_, _ = d.store.Update(id, func(r *Record) {
			s := r.Forwarding[name]
			if s == nil {
				s = &ForwardState{}
				r.Forwarding[name] = s
			}
			s.Attempts++
			if ferr == nil {
				s.Delivered, s.URL, s.Error, s.NextAttempt = true, url, "", time.Time{}
				return
			}
			s.Error = ferr.Error()
			var permanent PermanentError
			if errors.As(ferr, &permanent) || s.Attempts >= d.MaxAttempts {
				s.GaveUp = true
			} else {
				s.NextAttempt = time.Now().Add(d.Backoff(s.Attempts))
			}
		})
		if d.OnAttempt != nil {
			d.OnAttempt(name, ferr)
		}
		if ferr == nil && d.OnDelivered != nil {
			d.OnDelivered(rec, name, url)
		}
		if ferr != nil {
			d.log.Warn("forward failed", "id", id, "to", name, "err", ferr)
		} else {
			d.log.Info("forwarded", "id", id, "to", name, "url", url)
		}
	}
}

// MARK: Shared formatting

func reportTitle(rec *Record) string {
	if strings.TrimSpace(rec.Title) != "" {
		return rec.Title
	}
	kind := rec.Kind
	if kind == "" {
		kind = "bug"
	}
	return strings.ToUpper(kind[:1]) + kind[1:] + " report"
}

func appLine(rec *Record) string {
	if rec.Context.App == nil {
		return rec.Project
	}
	parts := []string{rec.Context.App.Name}
	if rec.Context.App.Version != "" {
		parts = append(parts, rec.Context.App.Version)
	}
	if rec.Context.App.Build != "" {
		parts = append(parts, "("+rec.Context.App.Build+")")
	}
	return strings.Join(parts, " ")
}

// MARK: GitHub

type GitHubForwarder struct {
	cfg       GitHubConfig
	reportURL func(string) string
}

func (g *GitHubForwarder) Name() string { return "github" }

func (g *GitHubForwarder) api(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, g.cfg.APIURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return doHTTP(ctx, req)
}

func (g *GitHubForwarder) Forward(ctx context.Context, rec *Record, open FileOpener) (string, error) {
	repo := "/repos/" + g.cfg.Owner + "/" + g.cfg.Repo
	links := map[string]string{}
	if g.cfg.AttachmentBranch != "" {
		if err := g.ensureBranch(ctx, repo); err != nil {
			return "", err
		}
		for _, f := range rec.Files {
			if !f.IsImage() {
				continue
			}
			data, err := open(f)
			if err != nil {
				return "", err
			}
			path := "snag/" + rec.ID + "/" + strings.TrimSuffix(f.Name, ".bin") + "-" + f.Filename
			_, err = g.api(ctx, "PUT", repo+"/contents/"+escapePath(path), map[string]string{
				"message": "Snag attachment for " + rec.ID, "content": base64.StdEncoding.EncodeToString(data), "branch": g.cfg.AttachmentBranch,
			})
			var perm PermanentError
			if err != nil && !(errors.As(err, &perm) && strings.Contains(err.Error(), "HTTP 422")) {
				return "", err
			}
			links[f.Name] = "https://github.com/" + g.cfg.Owner + "/" + g.cfg.Repo + "/blob/" + g.cfg.AttachmentBranch + "/" + escapePath(path) + "?raw=true"
		}
	}
	labels := append([]string{}, g.cfg.Labels...)
	if rec.Kind != "" && !contains(labels, rec.Kind) {
		labels = append(labels, rec.Kind)
	}
	for _, tag := range rec.Tags {
		if tag = normalizeLabel(tag); tag != "" && !contains(labels, tag) {
			labels = append(labels, tag)
		}
	}
	body, err := g.api(ctx, "POST", repo+"/issues", map[string]any{
		"title":  truncate(reportTitle(rec), 250),
		"body":   g.issueBody(rec, open, links),
		"labels": labels,
	})
	if err != nil {
		return "", err
	}
	var created struct {
		HTMLURL string `json:"html_url"`
	}
	_ = json.Unmarshal(body, &created)
	return created.HTMLURL, nil
}

func (g *GitHubForwarder) ensureBranch(ctx context.Context, repo string) error {
	if _, err := g.api(ctx, "GET", repo+"/git/ref/heads/"+escapePath(g.cfg.AttachmentBranch), nil); err == nil {
		return nil
	} else if !strings.Contains(err.Error(), "HTTP 404") {
		return err
	}
	data, err := g.api(ctx, "GET", repo, nil)
	if err != nil {
		return err
	}
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	_ = json.Unmarshal(data, &info)
	data, err = g.api(ctx, "GET", repo+"/git/ref/heads/"+escapePath(info.DefaultBranch), nil)
	if err != nil {
		return err
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	_ = json.Unmarshal(data, &ref)
	_, err = g.api(ctx, "POST", repo+"/git/refs", map[string]string{"ref": "refs/heads/" + g.cfg.AttachmentBranch, "sha": ref.Object.SHA})
	if err != nil && strings.Contains(err.Error(), "HTTP 422") {
		return nil // created concurrently
	}
	return err
}

func (g *GitHubForwarder) issueBody(rec *Record, open FileOpener, links map[string]string) string {
	var b strings.Builder
	if strings.TrimSpace(rec.Message) != "" {
		b.WriteString(rec.Message + "\n\n")
	}
	if rec.Contact != "" {
		b.WriteString("**Contact:** " + rec.Contact + "\n\n")
	}
	if u := g.reportURL(rec.ID); u != "" {
		b.WriteString("[Open in Snag](" + u + ")\n\n")
	}
	for _, f := range rec.Files {
		if f.IsImage() {
			if link, ok := links[f.Name]; ok {
				b.WriteString("![" + f.Filename + "](" + link + ")\n\n")
			}
		}
	}
	rows := contextRows(rec)
	if len(rows) > 0 {
		b.WriteString("<details><summary>Device and app</summary>\n\n| | |\n| --- | --- |\n")
		for _, row := range rows {
			b.WriteString("| " + mdCell(row[0]) + " | " + mdCell(row[1]) + " |\n")
		}
		b.WriteString("\n</details>\n\n")
	}
	for _, f := range rec.Files {
		if !f.IsText() {
			continue
		}
		data, err := open(f)
		if err != nil {
			continue
		}
		text := string(data)
		if len(text) > 6000 {
			text = "…\n" + text[len(text)-6000:]
		}
		fence := "```"
		if strings.Contains(text, "```") {
			fence = "~~~~"
		}
		b.WriteString("<details><summary>" + f.Filename + "</summary>\n\n" + fence + "\n" + text + "\n" + fence + "\n\n</details>\n\n")
	}
	b.WriteString("<!-- snag:" + rec.ID + " -->")
	out := b.String()
	if len(out) > 65000 {
		out = truncate(out, 64900) + "\n\n_(truncated)_"
	}
	return out
}

func mdCell(s string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = urlPathEscape(part)
	}
	return strings.Join(parts, "/")
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// MARK: Discord

type DiscordForwarder struct {
	webhook   string
	reportURL func(string) string
}

func (d *DiscordForwarder) Name() string { return "discord" }

func (d *DiscordForwarder) Forward(ctx context.Context, rec *Record, open FileOpener) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	var files []StoredFile
	for _, f := range rec.Files {
		if f.Size <= 10<<20 && len(files) < 10 {
			files = append(files, f)
		}
	}
	fields := []map[string]any{}
	if rec.Contact != "" {
		fields = append(fields, map[string]any{"name": "Contact", "value": truncate(rec.Contact, 1024)})
	}
	for i, row := range contextRows(rec) {
		if i >= 20 {
			break
		}
		fields = append(fields, map[string]any{"name": truncate(row[0], 256), "value": truncate(row[1], 1024), "inline": true})
	}
	color := 0x3E63DD
	if rec.Kind == "bug" {
		color = 0xE5484D
	}
	embed := map[string]any{
		"title":       truncate(reportTitle(rec), 256),
		"description": truncate(rec.Message, 4000),
		"timestamp":   rec.CreatedAt.Format(time.RFC3339),
		"footer":      map[string]string{"text": rec.Kind + " · " + rec.Project + " · " + rec.ID},
		"fields":      fields,
		"color":       color,
	}
	if u := d.reportURL(rec.ID); u != "" {
		embed["url"] = u
	}
	attachments := []map[string]any{}
	for i, f := range files {
		attachments = append(attachments, map[string]any{"id": i, "filename": f.Filename})
		if f.IsImage() && embed["image"] == nil {
			embed["image"] = map[string]string{"url": "attachment://" + f.Filename}
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"username": "Snag", "embeds": []any{embed}, "attachments": attachments,
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="payload_json"`)
	h.Set("Content-Type", "application/json")
	pw, _ := mw.CreatePart(h)
	_, _ = pw.Write(payload)
	for i, f := range files {
		data, err := open(f)
		if err != nil {
			return "", err
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files[%d]"; filename=%q`, i, f.Filename))
		h.Set("Content-Type", f.ContentType)
		pw, _ := mw.CreatePart(h)
		_, _ = pw.Write(data)
	}
	_ = mw.Close()
	target := d.webhook
	if strings.Contains(target, "?") {
		target += "&wait=true"
	} else {
		target += "?wait=true"
	}
	req, err := http.NewRequest("POST", target, &buf)
	if err != nil {
		return "", PermanentError{err}
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	_, err = doHTTP(ctx, req)
	return "", err
}

// MARK: Slack

type SlackForwarder struct {
	webhook   string
	reportURL func(string) string
}

func (s *SlackForwarder) Name() string { return "slack" }

func (s *SlackForwarder) Forward(ctx context.Context, rec *Record, _ FileOpener) (string, error) {
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	title := reportTitle(rec)
	blocks := []map[string]any{
		{"type": "header", "text": map[string]any{"type": "plain_text", "text": truncate(title, 150)}},
	}
	if strings.TrimSpace(rec.Message) != "" {
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(esc.Replace(rec.Message), 2900)}})
	}
	var lines []string
	for i, row := range contextRows(rec) {
		if i >= 10 {
			break
		}
		lines = append(lines, "*"+esc.Replace(row[0])+":* "+esc.Replace(row[1]))
	}
	if len(lines) > 0 {
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(strings.Join(lines, "\n"), 2900)}})
	}
	footer := rec.Kind + " · " + rec.Project
	if rec.Contact != "" {
		footer += " · " + esc.Replace(rec.Contact)
	}
	if u := s.reportURL(rec.ID); u != "" {
		footer += " · <" + u + "|Open in Snag>"
	}
	blocks = append(blocks, map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": truncate(footer, 2900)}}})
	payload, _ := json.Marshal(map[string]any{"text": title, "blocks": blocks})
	req, err := http.NewRequest("POST", s.webhook, bytes.NewReader(payload))
	if err != nil {
		return "", PermanentError{err}
	}
	req.Header.Set("Content-Type", "application/json")
	_, err = doHTTP(ctx, req)
	return "", err
}

// MARK: Webhook

// WebhookForwarder POSTs the stored record as JSON with attachments base64-encoded, signed like
// the Swift WebhookSink: X-Snag-Signature: sha256=HMAC(secret, "<timestamp>.<body>").
type WebhookForwarder struct{ cfg WebhookConfig }

func (w *WebhookForwarder) Name() string { return "webhook" }

func (w *WebhookForwarder) Forward(ctx context.Context, rec *Record, open FileOpener) (string, error) {
	type file struct {
		StoredFile
		Data string `json:"data"`
	}
	files := []file{}
	for _, f := range rec.Files {
		data, err := open(f)
		if err != nil {
			return "", err
		}
		files = append(files, file{f, base64.StdEncoding.EncodeToString(data)})
	}
	body, err := json.Marshal(map[string]any{"report": rec, "files": files})
	if err != nil {
		return "", PermanentError{err}
	}
	req, err := http.NewRequest("POST", w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return "", PermanentError{err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", rec.ID)
	if w.cfg.Secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req.Header.Set("X-Snag-Timestamp", ts)
		req.Header.Set("X-Snag-Signature", "sha256="+Sign(body, ts, w.cfg.Secret))
	}
	_, err = doHTTP(ctx, req)
	return "", err
}

// Sign is the webhook signature: hex HMAC-SHA256 of "<timestamp>.<body>".
func Sign(body []byte, timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// MARK: Email

type EmailForwarder struct {
	cfg       SMTPConfig
	reportURL func(string) string
}

// smtpSend delivers a message; tests replace it.
var smtpSend = sendSMTP

func (e *EmailForwarder) Name() string { return "email" }

func (e *EmailForwarder) Forward(ctx context.Context, rec *Record, open FileOpener) (string, error) {
	msg, err := BuildEmail(e.cfg, rec, open, e.reportURL(rec.ID))
	if err != nil {
		return "", err
	}
	return "", deliverMail(ctx, e.cfg, msg)
}

func deliverMail(ctx context.Context, cfg SMTPConfig, msg []byte) error {
	send := smtpSend
	done := make(chan error, 1)
	go func() { done <- send(cfg, msg) }()
	select {
	case err := <-done:
		var protoErr *textproto.Error
		if errors.As(err, &protoErr) && protoErr.Code >= 500 {
			return PermanentError{err}
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// BuildEmail renders a MIME message with the report as text and attachments as files.
func BuildEmail(cfg SMTPConfig, rec *Record, open FileOpener, link string) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	header := func(k, v string) { fmt.Fprintf(&buf, "%s: %s\r\n", k, v) }
	subject := "[" + appLine(rec) + "] " + reportTitle(rec)
	header("From", cfg.From)
	header("To", strings.Join(cfg.To, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", truncate(strings.ReplaceAll(subject, "\n", " "), 200)))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("Message-ID", "<"+rec.ID+"@snag>")
	if rec.Contact != "" && strings.Contains(rec.Contact, "@") && !strings.ContainsAny(rec.Contact, "\r\n<>,") {
		header("Reply-To", rec.Contact)
	}
	header("MIME-Version", "1.0")
	header("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	buf.WriteString("\r\n")

	var text strings.Builder
	if rec.Message != "" {
		text.WriteString(rec.Message + "\n\n")
	}
	if rec.Contact != "" {
		text.WriteString("Contact: " + rec.Contact + "\n")
	}
	if link != "" {
		text.WriteString("Open in Snag: " + link + "\n")
	}
	text.WriteString("\n")
	for _, row := range contextRows(rec) {
		text.WriteString(row[0] + ": " + row[1] + "\n")
	}
	text.WriteString("\nReport ID: " + rec.ID + "\n")

	h := textproto.MIMEHeader{}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Transfer-Encoding", "base64")
	pw, _ := mw.CreatePart(h)
	writeBase64Lines(pw, []byte(text.String()))
	for _, f := range rec.Files {
		data, err := open(f)
		if err != nil {
			return nil, err
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", f.ContentType)
		h.Set("Content-Transfer-Encoding", "base64")
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Filename}))
		pw, _ := mw.CreatePart(h)
		writeBase64Lines(pw, data)
	}
	_ = mw.Close()
	return buf.Bytes(), nil
}

func writeBase64Lines(w io.Writer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		_, _ = io.WriteString(w, enc[:76]+"\r\n")
		enc = enc[76:]
	}
	_, _ = io.WriteString(w, enc+"\r\n")
}

func sendSMTP(cfg SMTPConfig, msg []byte) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	var conn net.Conn
	var err error
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	if cfg.TLS == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.Host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if cfg.TLS == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return err
		}
	}
	if cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return PermanentError{err}
		}
	}
	if err := c.Mail(extractAddress(cfg.From)); err != nil {
		return err
	}
	for _, to := range cfg.To {
		if err := c.Rcpt(extractAddress(to)); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func extractAddress(s string) string {
	if i := strings.LastIndex(s, "<"); i >= 0 {
		if j := strings.LastIndex(s, ">"); j > i {
			return s[i+1 : j]
		}
	}
	return strings.TrimSpace(s)
}

// MARK: Context rows

var deviceLabels = []struct{ key, label string }{
	{"model", "Device"}, {"system", "OS"}, {"locale", "Locale"}, {"languages", "Languages"},
	{"timeZone", "Time zone"}, {"screen", "Screen"}, {"appearance", "Appearance"}, {"contentSize", "Text size"},
	{"accessibility", "Accessibility"}, {"batteryLevel", "Battery"}, {"lowPowerMode", "Low Power Mode"},
	{"thermalState", "Thermal state"}, {"network", "Network"}, {"freeDiskBytes", "Free disk"},
	{"memoryBytes", "Memory"}, {"uptimeSeconds", "Uptime"},
}

// contextRows flattens app, device and custom context into labeled rows.
func contextRows(rec *Record) [][2]string {
	var rows [][2]string
	if rec.Context.App != nil {
		rows = append(rows, [2]string{"App", appLine(rec)})
		if rec.Context.App.BundleIdentifier != "" {
			rows = append(rows, [2]string{"Bundle ID", rec.Context.App.BundleIdentifier})
		}
	}
	dev := rec.Context.Device
	seen := map[string]bool{"systemVersion": true}
	for _, item := range deviceLabels {
		v, ok := dev[item.key]
		if !ok {
			continue
		}
		seen[item.key] = true
		value := formatDeviceValue(item.key, v)
		if item.key == "system" {
			if ver, ok := dev["systemVersion"].(string); ok {
				value += " " + ver
			}
		}
		if value != "" {
			rows = append(rows, [2]string{item.label, value})
		}
	}
	var extra []string
	for k := range dev {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		rows = append(rows, [2]string{k, formatDeviceValue(k, dev[k])})
	}
	keys := make([]string, 0, len(rec.Context.Custom))
	for k := range rec.Context.Custom {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rows = append(rows, [2]string{k, rec.Context.Custom[k]})
	}
	return rows
}

func formatDeviceValue(key string, v any) string {
	switch val := v.(type) {
	case string:
		return val
	case bool:
		if val {
			return "on"
		}
		return "off"
	case float64:
		switch key {
		case "batteryLevel":
			return strconv.Itoa(int(val*100+0.5)) + "%"
		case "freeDiskBytes", "memoryBytes":
			return formatBytes(int64(val))
		case "uptimeSeconds":
			return (time.Duration(val) * time.Second).Round(time.Minute).String()
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(val))
		for _, item := range val {
			parts = append(parts, fmt.Sprint(item))
		}
		return strings.Join(parts, ", ")
	case nil:
		return ""
	default:
		data, _ := json.Marshal(val)
		return string(data)
	}
}

func formatBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// urlPathEscape escapes one path segment.
func urlPathEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// MARK: Replies

// BuildReply renders a plain-text email to a reporter.
func BuildReply(cfg SMTPConfig, to, subject, body, reportID string) ([]byte, error) {
	addr, err := mail.ParseAddress(to)
	if err != nil || strings.ContainsAny(to, "\r\n") {
		return nil, fmt.Errorf("%q isn't a valid email address", to)
	}
	subject = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(subject))
	if subject == "" {
		return nil, errors.New("the subject is empty")
	}
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("the message is empty")
	}
	var buf bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&buf, "%s: %s\r\n", k, v) }
	header("From", cfg.From)
	header("To", addr.String())
	if cfg.ReplyTo != "" {
		header("Reply-To", cfg.ReplyTo)
	}
	header("Subject", mime.QEncoding.Encode("utf-8", truncate(subject, 200)))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("Message-ID", "<reply-"+newID()+"@snag>")
	if reportID != "" {
		header("References", "<"+reportID+"@snag>")
	}
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "base64")
	buf.WriteString("\r\n")
	writeBase64Lines(&buf, []byte(strings.ReplaceAll(body, "\r\n", "\n")))
	return buf.Bytes(), nil
}

// SendReply emails a reporter through the server's SMTP settings.
func SendReply(ctx context.Context, cfg SMTPConfig, to string, msg []byte) error {
	cfg.To = []string{to}
	return deliverMail(ctx, cfg, msg)
}
