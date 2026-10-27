package server

import (
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*.html web/style.css
var webFS embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"ago": func(t time.Time) string {
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return strconv.Itoa(int(d.Minutes())) + "m ago"
		case d < 48*time.Hour:
			return strconv.Itoa(int(d.Hours())) + "h ago"
		default:
			return t.Format("Jan 2, 2006")
		}
	},
	"datetime":  func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
	"bytes":     func(n int64) string { return formatBytes(n) },
	"rows":      contextRows,
	"appLine":   appLine,
	"title":     reportTitle,
	"pathEsc":   url.PathEscape,
	"preview":   func(s string) string { s = strings.Join(strings.Fields(s), " "); return truncate(s, 160) },
	"hasPrefix": strings.HasPrefix,
}).ParseFS(webFS, "web/*.html"))

const dashboardCSP = "default-src 'none'; img-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	data["CSRF"] = s.csrfToken()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", dashboardCSP)
	w.Header().Set("Cache-Control", "no-store")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("rendering", "template", name, "err", err)
	}
}

func (s *Server) handleStyle(w http.ResponseWriter, r *http.Request) {
	data, _ := webFS.ReadFile("web/style.css")
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

const pageSize = 50

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	if status == "" {
		status = StatusOpen
	}
	filterStatus := status
	if status == "all" {
		filterStatus = ""
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	records, total := s.store.List(Filter{
		Status: filterStatus, Kind: q.Get("kind"), App: q.Get("app"), Query: q.Get("q"),
		Limit: pageSize, Offset: (page - 1) * pageSize,
	})
	apps, kinds := s.store.Facets()
	_, openCount := s.store.List(Filter{Status: StatusOpen})
	next := ""
	if page*pageSize < total {
		v := url.Values{}
		for k, vals := range q {
			v[k] = vals
		}
		v.Set("page", strconv.Itoa(page+1))
		next = "?" + v.Encode()
	}
	s.render(w, "list.html", map[string]any{
		"Reports": records, "Total": total, "Open": openCount,
		"Status": status, "Kind": q.Get("kind"), "App": q.Get("app"), "Query": q.Get("q"),
		"Apps": apps, "Kinds": kinds, "Next": next, "Forwarders": s.forwarder.Names(),
	})
}

func (s *Server) handleReportPage(w http.ResponseWriter, r *http.Request) {
	rec, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var texts []map[string]any
	for _, f := range rec.Files {
		if !f.IsText() {
			continue
		}
		path, _, err := s.store.FilePath(rec.ID, f.Name)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		const limit = 200_000
		truncated := false
		if len(data) > limit {
			data, truncated = data[len(data)-limit:], true
		}
		texts = append(texts, map[string]any{"File": f, "Text": string(data), "Truncated": truncated})
	}
	s.render(w, "report.html", map[string]any{"R": rec, "Texts": texts, "Forwarders": s.forwarder.Names()})
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	path, file, err := s.store.FilePath(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	disposition := "attachment"
	if file.IsImage() || (file.IsText() && r.URL.Query().Get("download") == "") {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", file.ContentType)
	w.Header().Set("Content-Disposition", disposition+`; filename="`+file.Filename+`"`)
	http.ServeFile(w, r, path)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := r.FormValue("status")
	if status != StatusOpen && status != StatusClosed {
		http.Error(w, "bad status", http.StatusBadRequest)
		return
	}
	if _, err := s.store.Update(r.PathValue("id"), func(rec *Record) { rec.Status = status }); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, safeReturn(r.FormValue("return"), "/reports/"+url.PathEscape(r.PathValue("id"))), http.StatusSeeOther)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Delete(r.PathValue("id")); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleRetryForward(w http.ResponseWriter, r *http.Request) {
	if err := s.forwarder.Retry(r.PathValue("id")); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/reports/"+url.PathEscape(r.PathValue("id")), http.StatusSeeOther)
}

// safeReturn only allows local paths, so a form can't redirect off-site.
func safeReturn(target, fallback string) string {
	if strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") && !strings.Contains(target, "\\") {
		return target
	}
	return fallback
}
