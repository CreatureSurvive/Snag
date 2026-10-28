package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is the Snag HTTP server: ingest API, dashboard and forwarding.
type Server struct {
	cfg       Config
	store     *Store
	projects  *ProjectStore
	issues    *IssueStore
	forwarder *Dispatcher
	notifier  *Notifier
	github    *GitHubSync
	attestor  *Attestor
	symbols   *SymbolStore
	// boardSecret keys voter hashes on the ideas board.
	boardSecret []byte
	log         *slog.Logger
	limiter     *rateLimiter
	csrfKey     []byte
	mux         *http.ServeMux
}

// Open loads everything under cfg.DataDir, migrates older data, and returns a ready server.
func Open(cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	projects, err := OpenProjects(cfg.DataDir, cfg.Seeds)
	if err != nil {
		return nil, err
	}
	issues, err := OpenIssues(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	attestor, err := NewAttestor(cfg.DataDir, nil)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	boardSecret, err := loadOrCreateKey(filepath.Join(cfg.DataDir, "board.key"))
	if err != nil {
		return nil, err
	}
	s := &Server{
		boardSecret: boardSecret,
		attestor:    attestor,
		symbols:     OpenSymbols(cfg.DataDir),
		cfg:         cfg,
		store:       store,
		projects:    projects,
		issues:      issues,
		log:         logger,
		limiter:     newRateLimiter(cfg.RatePerHour, time.Hour),
		csrfKey:     key,
		mux:         http.NewServeMux(),
	}
	s.forwarder = NewDispatcher(store, projects, cfg, s.reportURL, logger)
	s.notifier = NewNotifier(logger, cfg.NotifyPerHour)
	s.github = NewGitHubSync(issues, projects, cfg, logger)
	s.forwarder.OnDelivered = func(rec *Record, forwarder, url string) {
		if forwarder == "github" {
			s.github.Link(rec.Project, rec.Issue, url)
		}
	}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrating reports: %w", err)
	}
	issues.Refingerprint(func(i *Issue) *Record {
		if len(i.Reports) == 0 {
			return nil
		}
		rec, err := store.Get(i.Reports[0])
		if err != nil {
			return nil
		}
		return rec
	})
	s.routes()
	return s, nil
}

// migrate files reports from before projects and issues existed: each gets a project (from
// its old app name) and an issue of its own.
func (s *Server) migrate() error {
	for _, rec := range s.store.All() {
		if rec.Project != "" && rec.Issue != 0 {
			continue
		}
		slug := rec.Project
		if slug == "" {
			slug = Slugify(rec.App)
			if slug == "" {
				slug = "default"
			}
		}
		if _, ok := s.projects.Get(slug); !ok {
			name := rec.App
			if name == "" {
				name = slug
			}
			if _, err := s.projects.Create(name, slug); err != nil && !errors.Is(err, ErrProjectExists) {
				return err
			}
		}
		result, err := s.issues.File(slug, rec, false)
		if err != nil {
			return err
		}
		issue := result.Issue
		if rec.Status == "closed" {
			_, _ = s.issues.Update(slug, issue.Number, func(i *Issue) error { return SetStatus(i, IssueResolved) })
		}
		if _, err := s.store.Update(rec.ID, func(r *Record) { r.Project, r.Issue, r.Status = slug, issue.Number, "" }); err != nil {
			return err
		}
		s.log.Info("migrated report", "id", rec.ID, "project", slug, "issue", issue.Number)
	}
	return nil
}

// Dispatcher exposes the forwarding worker (started by main).
func (s *Server) Dispatcher() *Dispatcher { return s.forwarder }
func (s *Server) Store() *Store           { return s.store }
func (s *Server) Projects() *ProjectStore { return s.projects }
func (s *Server) Issues() *IssueStore     { return s.issues }
func (s *Server) Notifier() *Notifier     { return s.notifier }
func (s *Server) GitHub() *GitHubSync     { return s.github }
func (s *Server) Symbols() *SymbolStore   { return s.symbols }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
	})
	s.mux.HandleFunc("POST /api/v1/reports", s.handleIngest)
	s.mux.HandleFunc("OPTIONS /api/v1/reports", s.handleReportsPreflight)
	s.mux.HandleFunc("GET /sdk/snag.js", s.handleWebSDK)
	s.mux.HandleFunc("GET /b/{slug}", s.handleBoard)
	s.mux.HandleFunc("POST /b/{slug}/ideas/{n}/vote", s.handleBoardVote)
	s.mux.HandleFunc("GET /api/v1/board", s.handleAPIBoard)
	s.mux.HandleFunc("POST /api/v1/board/{n}/vote", s.handleAPIBoardVote)
	s.mux.HandleFunc("GET /api/v1/reports", s.admin(s.handleAPIList))
	s.mux.HandleFunc("GET /api/v1/reports/{id}", s.admin(s.handleAPIGet))
	s.mux.HandleFunc("POST /api/v1/github/webhook", s.handleGitHubWebhook)
	s.mux.HandleFunc("POST /api/v1/symbols", s.handleSymbolsUpload)
	s.mux.HandleFunc("POST /api/v1/inbound-email", s.handleInboundEmail)
	s.mux.HandleFunc("POST /api/v1/attest/challenge", s.handleAttestChallenge)
	s.mux.HandleFunc("POST /api/v1/attest/keys", s.handleAttestKey)
	s.mux.HandleFunc("GET /api/v1/followup/{id}", s.handleFollowUp)
	s.mux.HandleFunc("POST /api/v1/followup/{id}/messages", s.handleFollowUpReply)
	s.mux.HandleFunc("GET /api/v1/projects", s.admin(s.handleAPIProjects))
	s.mux.HandleFunc("GET /api/v1/projects/{slug}/issues", s.admin(s.handleAPIIssues))
	s.mux.HandleFunc("GET /api/v1/projects/{slug}/issues/{n}", s.admin(s.handleAPIIssue))
	s.mux.HandleFunc("POST /api/v1/projects/{slug}/issues/{n}/messages", s.admin(s.handleAPIThreadMessage))

	s.mux.HandleFunc("GET /static/style.css", s.handleStyle)
	s.mux.HandleFunc("GET /{$}", s.admin(s.handleOverview))
	s.mux.HandleFunc("GET /projects/new", s.admin(s.handleNewProject))
	s.mux.HandleFunc("POST /projects", s.admin(s.csrf(s.handleCreateProject)))
	s.mux.HandleFunc("GET /p/{slug}", s.admin(s.project(s.handleIssues)))
	s.mux.HandleFunc("POST /p/{slug}/bulk", s.admin(s.csrf(s.project(s.handleBulk))))
	s.mux.HandleFunc("GET /p/{slug}/releases", s.admin(s.project(s.handleReleases)))
	s.mux.HandleFunc("GET /p/{slug}/settings", s.admin(s.project(s.handleSettings)))
	s.mux.HandleFunc("POST /p/{slug}/settings", s.admin(s.csrf(s.project(s.handleSaveSettings))))
	s.mux.HandleFunc("POST /p/{slug}/keys", s.admin(s.csrf(s.project(s.handleCreateKey))))
	s.mux.HandleFunc("POST /p/{slug}/notify/test", s.admin(s.csrf(s.project(s.handleTestNotification))))
	s.mux.HandleFunc("POST /p/{slug}/keys/{id}/revoke", s.admin(s.csrf(s.project(s.handleRevokeKey))))
	s.mux.HandleFunc("POST /p/{slug}/symbol-keys", s.admin(s.csrf(s.project(s.handleCreateSymbolKey))))
	s.mux.HandleFunc("POST /p/{slug}/symbols", s.admin(s.csrf(s.project(s.handleSymbolsForm))))
	s.mux.HandleFunc("POST /p/{slug}/symbols/{uuid}/delete", s.admin(s.csrf(s.project(s.handleDeleteSymbols))))
	s.mux.HandleFunc("POST /p/{slug}/delete", s.admin(s.csrf(s.project(s.handleDeleteProject))))
	s.mux.HandleFunc("GET /p/{slug}/issues/{n}", s.admin(s.project(s.handleIssue)))
	s.mux.HandleFunc("POST /p/{slug}/issues/{n}/update", s.admin(s.csrf(s.project(s.handleUpdateIssue))))
	s.mux.HandleFunc("POST /p/{slug}/issues/{n}/notes", s.admin(s.csrf(s.project(s.handleAddNote))))
	s.mux.HandleFunc("POST /p/{slug}/issues/{n}/notes/{id}/delete", s.admin(s.csrf(s.project(s.handleDeleteNote))))
	s.mux.HandleFunc("POST /p/{slug}/issues/{n}/merge", s.admin(s.csrf(s.project(s.handleMerge))))
	s.mux.HandleFunc("POST /p/{slug}/issues/{n}/reply", s.admin(s.csrf(s.project(s.handleReply))))
	s.mux.HandleFunc("POST /p/{slug}/issues/{n}/thread", s.admin(s.csrf(s.project(s.handleThreadMessage))))
	s.mux.HandleFunc("POST /p/{slug}/issues/{n}/forward", s.admin(s.csrf(s.project(s.handleRetryForward))))
	s.mux.HandleFunc("POST /p/{slug}/issues/{n}/delete", s.admin(s.csrf(s.project(s.handleDeleteIssue))))
	s.mux.HandleFunc("POST /p/{slug}/reports/{id}/delete", s.admin(s.csrf(s.project(s.handleDeleteReport))))
	s.mux.HandleFunc("POST /backup", s.admin(s.csrf(s.handleBackup)))
	s.mux.HandleFunc("POST /p/{slug}/export", s.admin(s.csrf(s.project(s.handleExport))))
	s.mux.HandleFunc("GET /reports/{id}", s.admin(s.handleReportRedirect))
	s.mux.HandleFunc("GET /reports/{id}/files/{name}", s.admin(s.handleFile))
}

// issueURL is the dashboard link for an issue, or "" without SNAG_PUBLIC_URL.
func (s *Server) issueURL(project string, number int) string {
	if s.cfg.PublicURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/p/%s/issues/%d", s.cfg.PublicURL, project, number)
}

// reportURL links a report to its issue page.
func (s *Server) reportURL(id string) string {
	rec, err := s.store.Get(id)
	if err != nil || rec.Project == "" || rec.Issue == 0 {
		return ""
	}
	return s.issueURL(rec.Project, rec.Issue)
}

// MARK: Ingest

type apiError struct {
	Error string `json:"error"`
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSONResponse(w, status, apiError{Error: fmt.Sprintf(format, args...)})
}

func (s *Server) authenticateProject(r *http.Request) (*Project, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, false
	}
	return s.projects.Authenticate(strings.TrimSpace(token))
}

const maxAttachments = 20

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	project, ok := s.authenticateProject(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "missing or invalid API key")
		return
	}
	if !s.checkOrigin(w, r, project) {
		return
	}
	// A retry of a report we already have: answer without reading the body
	// or counting it against the rate limit.
	if key := r.Header.Get("Idempotency-Key"); ValidID(key) {
		if existing, err := s.store.Get(key); err == nil && existing.Project == project.Slug {
			s.respondExisting(w, existing)
			return
		}
	}
	ip := s.clientIP(r)
	if !s.limiter.allow(project.Slug + "|" + ip) {
		w.Header().Set("Retry-After", "600")
		fail(w, http.StatusTooManyRequests, "too many reports from this address; try again later")
		return
	}
	limited := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	// The body's hash is what an App Attest assertion signs.
	bodyHash := sha256.New()
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.TeeReader(limited, bodyHash), limited}
	reader, err := r.MultipartReader()
	if err != nil {
		fail(w, http.StatusBadRequest, "expected multipart/form-data")
		return
	}
	staged, err := s.store.Stage()
	if err != nil {
		s.log.Error("staging upload", "err", err)
		fail(w, http.StatusInternalServerError, "storage unavailable")
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staged)
		}
	}()

	var manifest *Manifest
	parts := map[string]StoredFile{}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				fail(w, http.StatusRequestEntityTooLarge, "report is larger than %d MB", s.cfg.MaxUploadBytes>>20)
			} else {
				fail(w, http.StatusBadRequest, "malformed multipart body")
			}
			return
		}
		name := part.FormName()
		if name == "report" {
			if manifest != nil {
				fail(w, http.StatusBadRequest, "duplicate report part")
				return
			}
			manifest = &Manifest{}
			if err := json.NewDecoder(io.LimitReader(part, 1<<20)).Decode(manifest); err != nil {
				fail(w, http.StatusBadRequest, "report part isn't valid JSON: %v", err)
				return
			}
			continue
		}
		if len(parts) >= maxAttachments {
			fail(w, http.StatusBadRequest, "too many attachments (max %d)", maxAttachments)
			return
		}
		if !partName.MatchString(name) {
			_ = part.Close()
			continue
		}
		if _, dup := parts[name]; dup {
			fail(w, http.StatusBadRequest, "duplicate part %q", name)
			return
		}
		file, err := saveUploadedPart(staged, len(parts), part)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				fail(w, http.StatusRequestEntityTooLarge, "report is larger than %d MB", s.cfg.MaxUploadBytes>>20)
			} else {
				fail(w, http.StatusBadRequest, "reading %s: %v", name, err)
			}
			return
		}
		parts[name] = file
	}
	if manifest == nil {
		fail(w, http.StatusBadRequest, "missing report part")
		return
	}
	_, _ = io.Copy(io.Discard, r.Body) // the epilogue counts toward the hash
	attested, attestErr := false, error(nil)
	if project.Attest.Mode != AttestOff {
		keyID := r.Header.Get("X-Snag-Attest-Key")
		assertion, decodeErr := base64.StdEncoding.DecodeString(r.Header.Get("X-Snag-Assertion"))
		switch {
		case keyID == "":
			attestErr = errors.New("no App Attest assertion")
		case decodeErr != nil:
			attestErr = errors.New("assertion isn't base64")
		default:
			attestErr = s.attestor.VerifyAssertion(project, keyID, assertion, bodyHash.Sum(nil))
		}
		attested = attestErr == nil
		if !attested && project.Attest.Mode == AttestRequire {
			s.log.Warn("unattested report rejected", "project", project.Slug, "err", attestErr)
			fail(w, http.StatusForbidden, "this project only accepts reports from verified devices: %v", attestErr)
			return
		}
	}
	if err := validateManifest(manifest); err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}

	rec := &Record{
		Manifest:   *manifest,
		ReceivedAt: time.Now().UTC(),
		Project:    project.Slug,
		Attested:   attested,
		RemoteAddr: ip,
		UserAgent:  truncate(r.UserAgent(), 200),
	}
	rec.ID = strings.ToLower(rec.ID)
	readerToken, readerHash := newReaderToken()
	rec.ReaderTokenHash = readerHash
	for _, entry := range manifest.Attachments {
		file, ok := parts[entry.Part]
		if !ok {
			continue // declared but not sent
		}
		file.Filename = SanitizeFilename(entry.Filename)
		file.Role = entry.Role
		file.ContentType = trustedContentType(entry.ContentType, file.ContentType)
		rec.Files = append(rec.Files, file)
	}

	redactor := NewRedactor(project.Redaction)
	if err := redactor.Record(rec, staged); err != nil {
		s.log.Error("redacting upload", "err", err)
		fail(w, http.StatusInternalServerError, "couldn't save the report")
		return
	}
	if rec.ServerRedactions = redactor.Removed; rec.ServerRedactions > 0 {
		s.log.Warn("server redacted text the app sent unredacted", "project", project.Slug, "count", rec.ServerRedactions)
	}

	s.symbolicateIncoming(project.Slug, rec, staged)

	filed, err := s.issues.File(project.Slug, rec, project.GroupDuplicates)
	issue, newIssue := filed.Issue, filed.Created
	if err != nil {
		s.log.Error("filing report", "err", err)
		fail(w, http.StatusInternalServerError, "couldn't save the report")
		return
	}
	rec.Issue = issue.Number
	if newIssue {
		// Forward once per issue, not once per duplicate.
		rec.Forwarding = s.forwarder.InitialState(project.Slug)
	}

	stored, created, err := s.store.Commit(staged, rec)
	committed = true
	if err != nil {
		_ = s.issues.Unfile(project.Slug, issue.Number, rec.ID)
		s.log.Error("saving report", "err", err)
		fail(w, http.StatusInternalServerError, "couldn't save the report")
		return
	}
	if !created {
		// Lost a race with a concurrent upload of the same report. Undo this filing unless it
		// landed in the same issue as the stored copy (then the report ID belongs there).
		if stored.Issue != issue.Number || stored.Project != project.Slug {
			_ = s.issues.Unfile(project.Slug, issue.Number, rec.ID)
		}
		if stored.Project != project.Slug {
			fail(w, http.StatusConflict, "this report ID is already in use")
			return
		}
		s.respondExisting(w, stored)
		return
	}
	s.log.Info("report received", "id", stored.ID, "project", project.Slug, "issue", stored.Issue, "new_issue", newIssue, "regressed", filed.Regressed, "files", len(stored.Files))
	if len(stored.Forwarding) > 0 {
		s.forwarder.Enqueue(stored.ID)
	}
	s.notifier.Notify(project, stored.Kind, alertFor(project, stored, filed, s.issueURL(project.Slug, stored.Issue)))
	if filed.Regressed {
		s.github.Push(project.Slug, issue.Number, GitHubChange{Status: true, AddLabels: []string{"regression"},
			Comment: fmt.Sprintf("Regressed: reported again in %s.", orUnknown(reportVersion(stored)))})
	}
	resp := s.ingestResponse(stored)
	resp["followUp"] = map[string]string{"token": readerToken, "path": "api/v1/followup/" + stored.ID}
	writeJSONResponse(w, http.StatusCreated, resp)
}

// respondExisting answers a retried upload. The original reader token was never stored, so a
// fresh one replaces it (the client that retried never received the first).
func (s *Server) respondExisting(w http.ResponseWriter, rec *Record) {
	token, hash := newReaderToken()
	if updated, err := s.store.Update(rec.ID, func(r *Record) { r.ReaderTokenHash = hash }); err == nil {
		rec = updated
	}
	resp := s.ingestResponse(rec)
	resp["followUp"] = map[string]string{"token": token, "path": "api/v1/followup/" + rec.ID}
	writeJSONResponse(w, http.StatusOK, resp)
}

func (s *Server) ingestResponse(rec *Record) map[string]any {
	out := map[string]any{"id": rec.ID, "project": rec.Project, "issue": rec.Issue}
	if u := s.issueURL(rec.Project, rec.Issue); u != "" {
		out["url"] = u
	}
	return out
}

var partName = regexp.MustCompile(`^attachment\d{1,3}$`)

func validateManifest(m *Manifest) error {
	if !ValidID(m.ID) {
		return errors.New("report id must be a UUID")
	}
	m.Title = truncate(strings.TrimSpace(m.Title), 300)
	m.Message = truncate(m.Message, 100_000)
	m.Contact = truncate(strings.TrimSpace(m.Contact), 300)
	m.Kind = truncate(strings.TrimSpace(m.Kind), 40)
	if m.Kind == "" {
		m.Kind = "bug"
	}
	if len(m.Tags) > 30 {
		m.Tags = m.Tags[:30]
	}
	for i := range m.Tags {
		m.Tags[i] = truncate(m.Tags[i], 60)
	}
	if len(m.Context.Custom) > 100 {
		return errors.New("too many custom context entries (max 100)")
	}
	if m.Title == "" && strings.TrimSpace(m.Message) == "" {
		return errors.New("a report needs a title or a message")
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

func saveUploadedPart(dir string, index int, part *multipart.Part) (StoredFile, error) {
	defer part.Close()
	name := fmt.Sprintf("%02d.bin", index)
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return StoredFile{}, err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, err := io.ReadFull(part, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return StoredFile{}, err
	}
	head = head[:n]
	if _, err := f.Write(head); err != nil {
		return StoredFile{}, err
	}
	rest, err := io.Copy(f, part)
	if err != nil {
		return StoredFile{}, err
	}
	return StoredFile{Name: name, Size: int64(n) + rest, ContentType: http.DetectContentType(head)}, nil
}

// trustedContentType keeps the client's declared type only when the bytes agree. Images must
// really be raster images (never SVG, which can carry script); text stays text.
func trustedContentType(declared, sniffed string) string {
	declared = strings.ToLower(strings.TrimSpace(declared))
	switch {
	case strings.HasPrefix(sniffed, "image/png"), strings.HasPrefix(sniffed, "image/jpeg"),
		strings.HasPrefix(sniffed, "image/gif"), strings.HasPrefix(sniffed, "image/webp"):
		return strings.SplitN(sniffed, ";", 2)[0]
	case strings.HasPrefix(sniffed, "video/mp4"):
		return "video/mp4"
	case strings.HasPrefix(sniffed, "text/plain"):
		if strings.HasPrefix(declared, "application/json") {
			return "application/json"
		}
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

var unsafeFilename = regexp.MustCompile(`[^A-Za-z0-9._\- ]`)

// SanitizeFilename keeps a display name safe for headers and file systems.
func SanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Trim(unsafeFilename.ReplaceAllString(name, ""), ". ")
	if name == "" {
		name = "attachment"
	}
	return truncate(name, 120)
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			first, _, _ := strings.Cut(fwd, ",")
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ip.String()
			}
		}
		if real := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); real != nil {
			return real.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// MARK: Admin auth

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminPassword == "" {
			http.Error(w, "The dashboard is disabled. Set SNAG_ADMIN_PASSWORD to enable it.", http.StatusForbidden)
			return
		}
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.AdminUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.AdminPassword)) == 1
		if !ok || !userOK || !passOK {
			if ok {
				// Slow down guessing.
				time.Sleep(300 * time.Millisecond)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="Snag", charset="UTF-8"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// csrfToken is an HMAC of the admin credentials; a cross-site page can't read or compute it.
func (s *Server) csrfToken() string {
	mac := hmac.New(sha256.New, s.csrfKey)
	mac.Write([]byte(s.cfg.AdminUser + "\x00" + s.cfg.AdminPassword))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(s.csrfToken())) != 1 {
			http.Error(w, "Invalid form token. Reload the page and try again.", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// MARK: Admin JSON API

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	records, total := s.store.List(Filter{Project: q.Get("project"), Kind: q.Get("kind"), Query: q.Get("q"), Limit: 200})
	writeJSONResponse(w, http.StatusOK, map[string]any{"total": total, "reports": records})
}

func (s *Server) handleAPIGet(w http.ResponseWriter, r *http.Request) {
	rec, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	writeJSONResponse(w, http.StatusOK, rec)
}

func (s *Server) handleAPIProjects(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Slug  string       `json:"slug"`
		Name  string       `json:"name"`
		Stats ProjectStats `json:"stats"`
	}
	var out []item
	for _, p := range s.projects.List() {
		out = append(out, item{p.Slug, p.Name, s.issues.Stats(p.Slug)})
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"projects": out})
}

func (s *Server) handleAPIIssues(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if _, ok := s.projects.Get(slug); !ok {
		fail(w, http.StatusNotFound, "no such project")
		return
	}
	issues, total := s.issues.List(slug, s.issueFilter(r, 200))
	writeJSONResponse(w, http.StatusOK, map[string]any{"total": total, "issues": issues})
}

func (s *Server) handleAPIIssue(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.PathValue("n"))
	issue, err := s.issues.Get(r.PathValue("slug"), n)
	if err != nil {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"issue": issue, "reports": s.store.ForIssue(issue.Reports)})
}

// MARK: Rate limiting

type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
	sweep  time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, hits: map[string][]time.Time{}}
}

func (l *rateLimiter) allow(key string) bool {
	if l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-l.window)
	if now.Sub(l.sweep) > l.window {
		for k, times := range l.hits {
			if len(times) == 0 || times[len(times)-1].Before(cutoff) {
				delete(l.hits, k)
			}
		}
		l.sweep = now
	}
	times := l.hits[key]
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	times = times[i:]
	if len(times) >= l.limit {
		l.hits[key] = times
		return false
	}
	l.hits[key] = append(times, now)
	return true
}

func orUnknown(v string) string {
	if v == "" {
		return "an unknown version"
	}
	return v
}

// pushDiff mirrors what changed between two versions of an issue to GitHub.
func (s *Server) pushDiff(before, after *Issue, comment string) {
	if after.GitHub == nil {
		return
	}
	change := GitHubChange{Comment: comment}
	if IsOpenStatus(before.Status) != IsOpenStatus(after.Status) || (before.Status != after.Status && (before.Status == IssueWontFix || after.Status == IssueWontFix)) {
		change.Status = true
	}
	for _, l := range after.Labels {
		if !contains(before.Labels, l) {
			change.AddLabels = append(change.AddLabels, l)
		}
	}
	for _, l := range before.Labels {
		if !contains(after.Labels, l) {
			change.RemoveLabels = append(change.RemoveLabels, l)
		}
	}
	if before.Priority != after.Priority {
		if l := githubPriorityLabel(after.Priority); l != "" {
			change.AddLabels = append(change.AddLabels, l)
		}
		if l := githubPriorityLabel(before.Priority); l != "" {
			change.RemoveLabels = append(change.RemoveLabels, l)
		}
	}
	if after.FixedIn != "" && after.FixedIn != before.FixedIn && comment == "" {
		change.Comment = "Fixed in " + after.FixedIn + "."
	}
	s.github.Push(after.Project, after.Number, change)
}
