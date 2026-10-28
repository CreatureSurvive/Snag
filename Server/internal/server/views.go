package server

import (
	"encoding/csv"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SavedView is a named issue-list filter (the list's query string).
type SavedView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Query string `json:"query"`
}

// viewQuery keeps only the list's filter parameters from a query string.
func viewQuery(raw string) string {
	in, _ := url.ParseQuery(strings.TrimPrefix(raw, "?"))
	out := url.Values{}
	for _, k := range []string{"status", "priority", "label", "kind", "version", "q", "sort", "regressed"} {
		if v := in.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	return out.Encode()
}

func (s *Server) handleSaveView(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	name := truncate(strings.TrimSpace(r.FormValue("name")), 40)
	query := viewQuery(r.FormValue("query"))
	if name == "" {
		http.Redirect(w, r, "/p/"+p.Slug+"?"+query, http.StatusSeeOther)
		return
	}
	_, _ = s.projects.Update(p.Slug, func(p *Project) error {
		for i, v := range p.Views {
			if strings.EqualFold(v.Name, name) {
				p.Views[i].Query = query
				return nil
			}
		}
		if len(p.Views) < 30 {
			p.Views = append(p.Views, SavedView{ID: newID(), Name: name, Query: query})
		}
		return nil
	})
	http.Redirect(w, r, "/p/"+p.Slug+"?"+query, http.StatusSeeOther)
}

func (s *Server) handleDeleteView(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	_, _ = s.projects.Update(p.Slug, func(p *Project) error {
		for i, v := range p.Views {
			if v.ID == r.PathValue("id") {
				p.Views = append(p.Views[:i], p.Views[i+1:]...)
				break
			}
		}
		return nil
	})
	http.Redirect(w, r, "/p/"+p.Slug, http.StatusSeeOther)
}

// handleIssuesCSV exports the issues matching the list's filters (all pages).
func (s *Server) handleIssuesCSV(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	f := s.issueFilter(r, 0)
	f.Offset = 0
	issues, _ := s.issues.List(p.Slug, f)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+p.Slug+`-issues-`+time.Now().UTC().Format("20060102")+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"number", "title", "kind", "status", "priority", "labels", "reports", "votes", "app_versions",
		"first_version", "fixed_in", "regressions", "created", "updated", "last_report", "url"})
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	for _, i := range issues {
		_ = cw.Write([]string{strconv.Itoa(i.Number), csvSafe(i.Title), i.Kind, i.Status, i.Priority, csvSafe(strings.Join(i.Labels, ", ")),
			strconv.Itoa(len(i.Reports)), strconv.Itoa(i.Votes), strings.Join(i.AppVersions, ", "), i.FirstVersion, i.FixedIn,
			strconv.Itoa(i.Regressions), stamp(i.CreatedAt), stamp(i.UpdatedAt), stamp(i.LastReportAt), s.issueURL(p.Slug, i.Number)})
	}
	cw.Flush()
}

// csvSafe stops spreadsheet apps from running a cell as a formula (titles come from users).
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// handleSearch searches every project.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	type group struct {
		P      *Project
		Issues []*Issue
		Total  int
	}
	var groups []group
	total := 0
	if query != "" {
		for _, p := range s.projects.List() {
			f := IssueFilter{Query: query, Limit: 20, Match: s.matchIssue}
			if r.URL.Query().Get("open") != "" {
				f.Status = "open"
			}
			issues, n := s.issues.List(p.Slug, f)
			if n > 0 {
				groups = append(groups, group{p, issues, n})
				total += n
			}
		}
		sort.SliceStable(groups, func(a, b int) bool { return groups[a].Total > groups[b].Total })
	}
	s.render(w, "search.html", map[string]any{"Query": query, "Groups": groups, "Total": total, "Open": r.URL.Query().Get("open") != ""})
}
