package server

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NotifyConfig is where and when a project pushes alerts to your phone.
type NotifyConfig struct {
	// Ntfy is a topic URL, such as https://ntfy.sh/my-secret-topic or a self-hosted server.
	Ntfy      string `json:"ntfy,omitempty"`
	NtfyToken string `json:"ntfyToken,omitempty"`
	// Pushover credentials: your user key and an application token.
	PushoverUser  string `json:"pushoverUser,omitempty"`
	PushoverToken string `json:"pushoverToken,omitempty"`

	OnNewIssue   bool `json:"onNewIssue"`
	OnRegression bool `json:"onRegression"`
	// OnReport also alerts for reports joining an existing open issue.
	OnReport bool `json:"onReport,omitempty"`
	// OnPriority alerts when an issue is set to high or urgent.
	OnPriority bool `json:"onPriority,omitempty"`
	// Kinds limits alerts to these report kinds (empty means all).
	Kinds []string `json:"kinds,omitempty"`
}

func (c NotifyConfig) Enabled() bool {
	return c.Ntfy != "" || (c.PushoverUser != "" && c.PushoverToken != "")
}

// Notification events.
const (
	EventNewIssue   = "new_issue"
	EventRegression = "regression"
	EventReport     = "report"
	EventPriority   = "priority"
	EventTest       = "test"
)

// Alert is one push notification.
type Alert struct {
	Event   string
	Title   string
	Message string
	URL     string
	// Urgent raises the priority (ntfy 5, Pushover 1).
	Urgent bool
}

func (c NotifyConfig) wants(event, kind string) bool {
	if len(c.Kinds) > 0 && event != EventTest && !contains(c.Kinds, kind) {
		return false
	}
	switch event {
	case EventNewIssue:
		return c.OnNewIssue
	case EventRegression:
		return c.OnRegression
	case EventReport:
		return c.OnReport
	case EventPriority:
		return c.OnPriority
	case EventTest:
		return true
	}
	return false
}

// Notifier sends alerts in the background, capped per project so a flood of reports doesn't
// become a flood of pushes.
type Notifier struct {
	log     *slog.Logger
	limiter *rateLimiter
	wg      sync.WaitGroup
	// PushoverURL is the Pushover API endpoint; tests replace it.
	PushoverURL string
	mu          sync.Mutex
	suppressed  map[string]int
}

func NewNotifier(logger *slog.Logger, perHour int) *Notifier {
	return &Notifier{log: logger, limiter: newRateLimiter(perHour, time.Hour), PushoverURL: "https://api.pushover.net/1/messages.json", suppressed: map[string]int{}}
}

// Notify sends alert for project p if its rules want it. It returns immediately.
func (n *Notifier) Notify(p *Project, kind string, alert Alert) {
	cfg := p.Notify
	if !cfg.Enabled() || !cfg.wants(alert.Event, kind) {
		return
	}
	if alert.Event != EventTest && !n.limiter.allow(p.Slug) {
		n.mu.Lock()
		n.suppressed[p.Slug]++
		n.mu.Unlock()
		return
	}
	n.mu.Lock()
	if dropped := n.suppressed[p.Slug]; dropped > 0 {
		alert.Message += fmt.Sprintf("\n(+%d more alerts were held back by the hourly limit)", dropped)
		n.suppressed[p.Slug] = 0
	}
	n.mu.Unlock()
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := n.Send(ctx, cfg, alert); err != nil {
			n.log.Warn("notification failed", "project", p.Slug, "event", alert.Event, "err", err)
		}
	}()
}

// Wait blocks until in-flight alerts finish (for tests and shutdown).
func (n *Notifier) Wait() { n.wg.Wait() }

// Send delivers to every configured channel, retrying each briefly.
func (n *Notifier) Send(ctx context.Context, cfg NotifyConfig, alert Alert) error {
	var errs []string
	try := func(name string, send func() error) {
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if err = send(); err == nil {
				return
			}
			if _, permanent := err.(PermanentError); permanent {
				break
			}
			select {
			case <-ctx.Done():
				errs = append(errs, name+": "+ctx.Err().Error())
				return
			case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
			}
		}
		errs = append(errs, name+": "+err.Error())
	}
	if cfg.Ntfy != "" {
		try("ntfy", func() error { return n.sendNtfy(ctx, cfg, alert) })
	}
	if cfg.PushoverUser != "" && cfg.PushoverToken != "" {
		try("pushover", func() error { return n.sendPushover(ctx, cfg, alert) })
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// headerSafe keeps a value on one line for an HTTP header.
func headerSafe(s string) string {
	return truncate(strings.Join(strings.Fields(s), " "), 250)
}

func (n *Notifier) sendNtfy(ctx context.Context, cfg NotifyConfig, alert Alert) error {
	req, err := http.NewRequest("POST", cfg.Ntfy, strings.NewReader(truncate(alert.Message, 3500)))
	if err != nil {
		return PermanentError{err}
	}
	req.Header.Set("Title", headerSafe(alert.Title))
	priority := "default"
	tags := "bug"
	switch {
	case alert.Urgent:
		priority = "urgent"
		tags = "rotating_light"
	case alert.Event == EventRegression:
		priority = "high"
		tags = "repeat"
	}
	req.Header.Set("Priority", priority)
	req.Header.Set("Tags", tags)
	if alert.URL != "" {
		req.Header.Set("Click", alert.URL)
	}
	if cfg.NtfyToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.NtfyToken)
	}
	_, err = doHTTP(ctx, req)
	return err
}

func (n *Notifier) sendPushover(ctx context.Context, cfg NotifyConfig, alert Alert) error {
	form := url.Values{
		"token":   {cfg.PushoverToken},
		"user":    {cfg.PushoverUser},
		"title":   {truncate(alert.Title, 250)},
		"message": {truncate(alert.Message, 1024)},
	}
	if alert.URL != "" {
		form.Set("url", alert.URL)
		form.Set("url_title", "Open in Snag")
	}
	priority := 0
	if alert.Urgent || alert.Event == EventRegression {
		priority = 1
	}
	form.Set("priority", strconv.Itoa(priority))
	req, err := http.NewRequest("POST", n.PushoverURL, bytes.NewReader([]byte(form.Encode())))
	if err != nil {
		return PermanentError{err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_, err = doHTTP(ctx, req)
	return err
}

// alertFor describes a filing result for a project's issue.
func alertFor(p *Project, rec *Record, result FileResult, link string) Alert {
	issue := result.Issue
	version := ""
	if v := reportVersion(rec); v != "" {
		version = " · " + v
	}
	body := strings.TrimSpace(rec.Message)
	if body == "" {
		body = issue.Title
	}
	switch {
	case result.Regressed:
		return Alert{Event: EventRegression, Title: fmt.Sprintf("%s #%d regressed%s", p.Name, issue.Number, version),
			Message: issue.Title + "\n" + body, URL: link}
	case result.Created:
		return Alert{Event: EventNewIssue, Title: fmt.Sprintf("%s #%d · new %s%s", p.Name, issue.Number, issue.Kind, version),
			Message: issue.Title + "\n" + body, URL: link}
	default:
		return Alert{Event: EventReport, Title: fmt.Sprintf("%s #%d · %d reports%s", p.Name, issue.Number, len(issue.Reports), version),
			Message: issue.Title + "\n" + body, URL: link}
	}
}
