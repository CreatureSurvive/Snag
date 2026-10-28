package server

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Version is the server's version, set by main.
var Version = "dev"

// Metrics are Prometheus counters kept in memory (they restart from zero with the server).
// Gauges (issues, reports, storage) are computed when /metrics is scraped.
type Metrics struct {
	mu       sync.Mutex
	counters map[string]map[string]float64 // name -> rendered labels -> value
	help     map[string]string
	started  time.Time
}

func NewMetrics() *Metrics {
	return &Metrics{counters: map[string]map[string]float64{}, help: map[string]string{}, started: time.Now()}
}

// Add increments a counter. labels alternate name, value.
func (m *Metrics) Add(name, help string, labels ...string) {
	if m == nil {
		return
	}
	var b strings.Builder
	for i := 0; i+1 < len(labels); i += 2 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", labels[i], labels[i+1])
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counters[name] == nil {
		m.counters[name] = map[string]float64{}
		m.help[name] = help
	}
	m.counters[name][b.String()]++
}

func (m *Metrics) write(b *strings.Builder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.counters))
	for n := range m.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", n, m.help[n], n)
		series := m.counters[n]
		keys := make([]string, 0, len(series))
		for k := range series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "" {
				fmt.Fprintf(b, "%s %g\n", n, series[k])
			} else {
				fmt.Fprintf(b, "%s{%s} %g\n", n, k, series[k])
			}
		}
	}
}

// statusWriter records the response status for the request counter.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func routeGroup(r *http.Request) string {
	p := r.URL.Path
	switch {
	case p == "/api/v1/reports" && r.Method == http.MethodPost:
		return "ingest"
	case strings.HasPrefix(p, "/api/v1/followup"):
		return "followup"
	case strings.HasPrefix(p, "/api/v1/symbols"):
		return "symbols"
	case strings.HasPrefix(p, "/api/"):
		return "api"
	case strings.HasPrefix(p, "/b/"):
		return "board"
	case strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, "/sdk/"):
		return "static"
	case p == "/metrics":
		return "metrics"
	}
	return "dashboard"
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	token, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if hasBearer {
		if s.cfg.MetricsToken == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(s.cfg.MetricsToken)) != 1 {
			fail(w, http.StatusUnauthorized, "invalid metrics token")
			return
		}
		s.writeMetrics(w, r)
		return
	}
	// Otherwise the admin's credentials (Basic auth or a signed-in browser).
	s.admin(s.writeMetrics)(w, r)
}

func (s *Server) writeMetrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP snag_build_info The running snag-server.\n# TYPE snag_build_info gauge\nsnag_build_info{version=%q} 1\n", Version)
	fmt.Fprintf(&b, "# HELP snag_uptime_seconds Seconds since the server started.\n# TYPE snag_uptime_seconds gauge\nsnag_uptime_seconds %d\n", int(time.Since(s.metrics.started).Seconds()))
	s.metrics.write(&b)

	usage := s.store.Usage()
	reports := map[string]int{}
	pending := 0
	for _, rec := range s.store.All() {
		reports[rec.Project]++
		for _, st := range rec.Forwarding {
			if st != nil && !st.Delivered && !st.GaveUp {
				pending++
			}
		}
	}
	b.WriteString("# HELP snag_issues Issues by project and status.\n# TYPE snag_issues gauge\n")
	projects := s.projects.List()
	for _, p := range projects {
		stats := s.issues.Stats(p.Slug)
		for _, st := range []string{IssueNew, IssueTriaged, IssueInProgress, IssueResolved, IssueWontFix} {
			fmt.Fprintf(&b, "snag_issues{project=%q,status=%q} %d\n", p.Slug, st, stats.ByStatus[st])
		}
	}
	b.WriteString("# HELP snag_reports Stored reports by project.\n# TYPE snag_reports gauge\n")
	for _, p := range projects {
		fmt.Fprintf(&b, "snag_reports{project=%q} %d\n", p.Slug, reports[p.Slug])
	}
	b.WriteString("# HELP snag_storage_bytes Attachment bytes by project.\n# TYPE snag_storage_bytes gauge\n")
	for _, p := range projects {
		fmt.Fprintf(&b, "snag_storage_bytes{project=%q} %d\n", p.Slug, usage[p.Slug])
	}
	b.WriteString("# HELP snag_storage_limit_bytes Each project's storage limit (0 = none).\n# TYPE snag_storage_limit_bytes gauge\n")
	for _, p := range projects {
		fmt.Fprintf(&b, "snag_storage_limit_bytes{project=%q} %d\n", p.Slug, s.storageLimit(p))
	}
	fmt.Fprintf(&b, "# HELP snag_forward_pending Deliveries waiting to be retried.\n# TYPE snag_forward_pending gauge\nsnag_forward_pending %d\n", pending)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}

// storageLimit is a project's attachment limit in bytes, or 0 for none.
func (s *Server) storageLimit(p *Project) int64 {
	mb := p.StorageLimitMB
	if mb == 0 {
		mb = s.cfg.ProjectStorageMB
	}
	return int64(mb) << 20
}

// enforceQuota keeps a report within its project's storage limit by dropping attachments,
// media first; the report's text is always kept.
func (s *Server) enforceQuota(p *Project, rec *Record, dir string) {
	limit := s.storageLimit(p)
	if limit <= 0 {
		return
	}
	used := s.store.Usage()[p.Slug]
	var incoming int64
	for _, f := range rec.Files {
		incoming += f.Size
	}
	if used+incoming <= limit {
		return
	}
	// Drop media first, then the rest, until it fits.
	order := append([]StoredFile{}, rec.Files...)
	sort.SliceStable(order, func(i, j int) bool { return !order[i].IsText() && order[j].IsText() })
	drop := map[string]bool{}
	for _, f := range order {
		if used+incoming <= limit {
			break
		}
		drop[f.Name] = true
		incoming -= f.Size
	}
	kept := rec.Files[:0]
	for _, f := range rec.Files {
		if drop[f.Name] {
			_ = os.Remove(filepath.Join(dir, f.Name))
			rec.DroppedForQuota++
			continue
		}
		kept = append(kept, f)
	}
	rec.Files = kept
	s.metrics.Add("snag_attachments_dropped_total", "Attachments not kept because a project was over its storage limit.", "project", p.Slug)
	s.log.Warn("project over its storage limit; attachments dropped", "project", p.Slug, "dropped", rec.DroppedForQuota)
}
