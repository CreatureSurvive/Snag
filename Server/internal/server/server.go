package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	"strings"
	"sync"
	"time"
)

// Server is the Snag HTTP server: ingest API, dashboard and forwarding.
type Server struct {
	cfg       Config
	store     *Store
	forwarder *Dispatcher
	log       *slog.Logger
	limiter   *rateLimiter
	csrfKey   []byte
	mux       *http.ServeMux
}

func New(cfg Config, store *Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	s := &Server{
		cfg:     cfg,
		store:   store,
		log:     logger,
		limiter: newRateLimiter(cfg.RatePerHour, time.Hour),
		csrfKey: key,
		mux:     http.NewServeMux(),
	}
	s.forwarder = NewDispatcher(store, BuildForwarders(cfg, s.reportURL), logger)
	s.routes()
	return s
}

// Dispatcher exposes the forwarding worker (started by Run).
func (s *Server) Dispatcher() *Dispatcher { return s.forwarder }

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
	s.mux.HandleFunc("GET /api/v1/reports", s.admin(s.handleAPIList))
	s.mux.HandleFunc("GET /api/v1/reports/{id}", s.admin(s.handleAPIGet))

	s.mux.HandleFunc("GET /{$}", s.admin(s.handleDashboard))
	s.mux.HandleFunc("GET /reports/{id}", s.admin(s.handleReportPage))
	s.mux.HandleFunc("GET /reports/{id}/files/{name}", s.admin(s.handleFile))
	s.mux.HandleFunc("POST /reports/{id}/status", s.admin(s.csrf(s.handleStatus)))
	s.mux.HandleFunc("POST /reports/{id}/delete", s.admin(s.csrf(s.handleDelete)))
	s.mux.HandleFunc("POST /reports/{id}/forward", s.admin(s.csrf(s.handleRetryForward)))
	s.mux.HandleFunc("GET /static/style.css", s.handleStyle)
}

func (s *Server) reportURL(id string) string {
	if s.cfg.PublicURL == "" {
		return ""
	}
	return s.cfg.PublicURL + "/reports/" + id
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

func (s *Server) authenticateApp(r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return "", false
	}
	token = strings.TrimSpace(token)
	var app string
	found := 0
	// Compare against every key so timing doesn't reveal which prefix matched.
	for key, name := range s.cfg.APIKeys {
		if subtle.ConstantTimeCompare([]byte(key), []byte(token)) == 1 {
			app = name
			found = 1
		}
	}
	return app, found == 1
}

const maxAttachments = 20

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	app, ok := s.authenticateApp(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "missing or invalid API key")
		return
	}
	// A retry of a report we already have: answer without reading the body
	// or counting it against the rate limit.
	if key := r.Header.Get("Idempotency-Key"); ValidID(key) {
		if existing, err := s.store.Get(key); err == nil {
			writeJSONResponse(w, http.StatusOK, s.ingestResponse(existing))
			return
		}
	}
	ip := s.clientIP(r)
	if !s.limiter.allow(ip) {
		w.Header().Set("Retry-After", "600")
		fail(w, http.StatusTooManyRequests, "too many reports from this address; try again later")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
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
	if err := validateManifest(manifest); err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}

	rec := &Record{
		Manifest:   *manifest,
		ReceivedAt: time.Now().UTC(),
		App:        app,
		Status:     StatusOpen,
		RemoteAddr: ip,
		UserAgent:  truncate(r.UserAgent(), 200),
	}
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
	rec.Forwarding = s.forwarder.InitialState()

	stored, created, err := s.store.Commit(staged, rec)
	committed = true
	if err != nil {
		s.log.Error("saving report", "err", err)
		fail(w, http.StatusInternalServerError, "couldn't save the report")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		s.log.Info("report received", "id", stored.ID, "app", app, "kind", stored.Kind, "files", len(stored.Files))
		s.forwarder.Enqueue(stored.ID)
	}
	writeJSONResponse(w, status, s.ingestResponse(stored))
}

func (s *Server) ingestResponse(rec *Record) map[string]any {
	out := map[string]any{"id": rec.ID}
	if u := s.reportURL(rec.ID); u != "" {
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
	records, total := s.store.List(Filter{Status: q.Get("status"), Kind: q.Get("kind"), App: q.Get("app"), Query: q.Get("q"), Limit: 200})
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
