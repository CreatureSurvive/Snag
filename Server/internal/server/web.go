package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*.html web/style.css
var webFS embed.FS

// cssVersion changes whenever the stylesheet does, so browsers never use a stale copy.
var cssVersion = func() string {
	data, _ := webFS.ReadFile("web/style.css")
	return hashKey(string(data))[:10]
}()

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"cssVersion": func() string { return cssVersion },
	"ago": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
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
	"datetime":    func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
	"day":         func(t time.Time) string { return t.Format("Jan 2") },
	"bytes":       func(n int64) string { return formatBytes(n) },
	"rows":        contextRows,
	"appLine":     appLine,
	"preview":     func(s string) string { s = strings.Join(strings.Fields(s), " "); return truncate(s, 160) },
	"statusLabel": StatusLabel,
	"isOpen":      IsOpenStatus,
	"percent":     func(f float64) int { return int(f*100 + 0.5) },
	"join":        strings.Join,
	"add":         func(a, b int) int { return a + b },
	"sub":         func(a, b int) int { return a - b },
	"mul":         func(a, b int) int { return a * b },
	"max1":        func(n int) int { return max(n, 1) },
	"statuses":    func() []string { return Statuses },
	"priorities":  func() []string { return Priorities[1:] },
	"query": func(base url.Values, kv ...string) string {
		v := url.Values{}
		for k, vals := range base {
			v[k] = append([]string{}, vals...)
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == "" {
				v.Del(kv[i])
			} else {
				v.Set(kv[i], kv[i+1])
			}
		}
		v.Del("page")
		if enc := v.Encode(); enc != "" {
			return "?" + enc
		}
		return "?"
	},
	"canReply": func(contact string) bool { return strings.Contains(contact, "@") },
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
		return m
	},
}).ParseFS(webFS, "web/*.html"))

const dashboardCSP = "default-src 'none'; img-src 'self'; media-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	data["CSRF"] = s.csrfToken()
	data["AllProjects"] = s.projects.List()
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
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

type projectKey struct{}

// project resolves {slug} and passes the project in the request context.
func (s *Server) project(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.projects.Get(r.PathValue("slug"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), projectKey{}, p)))
	}
}

func projectFrom(r *http.Request) *Project { return r.Context().Value(projectKey{}).(*Project) }

func (s *Server) issueFrom(w http.ResponseWriter, r *http.Request) (*Issue, bool) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	issue, err := s.issues.Get(projectFrom(r).Slug, n)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	return issue, true
}

func issuePath(p string, n int) string { return fmt.Sprintf("/p/%s/issues/%d", p, n) }

func (s *Server) back(w http.ResponseWriter, r *http.Request, fallback string) {
	http.Redirect(w, r, safeReturn(r.FormValue("return"), fallback), http.StatusSeeOther)
}

// MARK: Overview

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	type card struct {
		P     *Project
		Stats ProjectStats
	}
	var cards []card
	for _, p := range s.projects.List() {
		cards = append(cards, card{p, s.issues.Stats(p.Slug)})
	}
	s.render(w, "overview.html", map[string]any{"Cards": cards})
}

func (s *Server) handleNewProject(w http.ResponseWriter, r *http.Request) {
	s.render(w, "newproject.html", map[string]any{})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.projects.Create(r.FormValue("name"), strings.TrimSpace(r.FormValue("slug")))
	if err != nil {
		s.render(w, "newproject.html", map[string]any{"Error": err.Error(), "Name": r.FormValue("name"), "Slug": r.FormValue("slug")})
		return
	}
	key, err := s.projects.AddKey(p.Slug, "Default")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderSettings(w, p, key, "")
}

// MARK: Issue list

func (s *Server) issueFilter(r *http.Request, limit int) IssueFilter {
	q := r.URL.Query()
	status := q.Get("status")
	if status == "" {
		status = "open"
	} else if status == "all" {
		status = ""
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	return IssueFilter{
		Status: status, Priority: q.Get("priority"), Label: q.Get("label"), Kind: q.Get("kind"),
		Version: q.Get("version"), Query: q.Get("q"), Sort: q.Get("sort"), Regressed: q.Get("regressed") != "",
		Limit: limit, Offset: (page - 1) * limit,
		Match: func(issue *Issue, query string) bool { return s.store.Matches(issue.Reports, query) },
	}
}

const pageSize = 50

func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	f := s.issueFilter(r, pageSize)
	issues, total := s.issues.List(p.Slug, f)
	labels, kinds, versions := s.issues.Facets(p.Slug)
	q := r.URL.Query()
	status := q.Get("status")
	if status == "" {
		status = "open"
	}
	next := ""
	if f.Offset+pageSize < total {
		v := url.Values{}
		for k, vals := range q {
			v[k] = vals
		}
		page, _ := strconv.Atoi(q.Get("page"))
		v.Set("page", strconv.Itoa(max(page, 1)+1))
		next = "?" + v.Encode()
	}
	s.render(w, "issues.html", map[string]any{
		"P": p, "Issues": issues, "Total": total, "Stats": s.issues.Stats(p.Slug),
		"Status": status, "Priority": q.Get("priority"), "Label": q.Get("label"), "Kind": q.Get("kind"),
		"Version": q.Get("version"), "Query": q.Get("q"), "Sort": q.Get("sort"), "Q": q, "Regressed": q.Get("regressed") != "",
		"Labels": labels, "Kinds": kinds, "Versions": versions, "Next": next,
		"Return": r.URL.RequestURI(),
	})
}

func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	_ = r.ParseForm()
	var numbers []int
	for _, v := range r.Form["issue"] {
		if n, err := strconv.Atoi(v); err == nil {
			numbers = append(numbers, n)
		}
	}
	fallback := "/p/" + p.Slug
	if len(numbers) == 0 {
		s.back(w, r, fallback)
		return
	}
	action := r.FormValue("action")
	switch {
	case action == "merge":
		into := numbers[0]
		for _, n := range numbers {
			if n < into {
				into = n // the oldest issue keeps its number
			}
		}
		if _, err := s.issues.Merge(p.Slug, into, numbers); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.pushMerged(p.Slug, into, numbers)
		http.Redirect(w, r, issuePath(p.Slug, into), http.StatusSeeOther)
		return
	case action == "delete":
		for _, n := range numbers {
			s.deleteIssue(p.Slug, n)
		}
	case strings.HasPrefix(action, "status:"), strings.HasPrefix(action, "priority:"), strings.HasPrefix(action, "label:"):
		kind, value, _ := strings.Cut(action, ":")
		if kind == "label" {
			value = r.FormValue("label")
		}
		for _, n := range numbers {
			original, _ := s.issues.Get(p.Slug, n)
			updated, err := s.issues.Update(p.Slug, n, func(i *Issue) error {
				switch kind {
				case "status":
					return SetStatus(i, value)
				case "priority":
					if value == "none" {
						value = ""
					}
					return SetPriority(i, value)
				default:
					AddLabel(i, value)
					return nil
				}
			})
			if err != nil && !errors.Is(err, ErrNotFound) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err == nil && original != nil {
				s.pushDiff(original, updated, "")
			}
		}
	}
	s.back(w, r, fallback)
}

// pushMerged closes the GitHub issues of merged-away issues as duplicates.
func (s *Server) pushMerged(project string, into int, from []int) {
	target, err := s.issues.Get(project, into)
	if err != nil {
		return
	}
	ref := fmt.Sprintf("Snag issue #%d", into)
	if target.GitHub != nil {
		ref = target.GitHub.URL
	}
	for _, n := range from {
		if n != into {
			s.github.Push(project, n, GitHubChange{Status: true, Comment: "Duplicate of " + ref + " (merged in Snag)."})
		}
	}
}

// MARK: Issue page

func (s *Server) handleIssue(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	issue, ok := s.issueFrom(w, r)
	if !ok {
		return
	}
	if issue.MergedInto != 0 {
		http.Redirect(w, r, issuePath(p.Slug, issue.MergedInto), http.StatusSeeOther)
		return
	}
	reports := s.store.ForIssue(issue.Reports)
	// Newest report first.
	for i, j := 0, len(reports)-1; i < j; i, j = i+1, j-1 {
		reports[i], reports[j] = reports[j], reports[i]
	}
	type view struct {
		R      *Record
		Texts  []map[string]any
		Crumbs []BreadcrumbRow
	}
	views := make([]view, 0, len(reports))
	for i, rec := range reports {
		v := view{R: rec}
		if i == 0 { // logs only for the newest, to keep the page light
			v.Texts = s.texts(rec)
			v.Crumbs = s.breadcrumbs(rec)
		}
		views = append(views, v)
	}
	text := func(i *Issue) string {
		var b strings.Builder
		b.WriteString(i.Title)
		for _, rec := range s.store.ForIssue(i.Reports) {
			b.WriteString(" " + rec.Message)
		}
		return b.String()
	}
	labels, _, _ := s.issues.Facets(p.Slug)
	activity := append([]Event{}, issue.Activity...)
	for i, j := 0, len(activity)-1; i < j; i, j = i+1, j-1 {
		activity[i], activity[j] = activity[j], activity[i]
	}
	s.render(w, "issue.html", map[string]any{
		"P": p, "I": issue, "Reports": views, "Similar": s.issues.Similar(issue, text, 5),
		"Activity": activity, "AllLabels": labels, "CanEmail": s.cfg.SMTP != nil,
		"Forwarders": s.forwarder.Names(p.Slug), "Self": issuePath(p.Slug, issue.Number),
		"Flash": r.URL.Query().Get("flash"),
	})
}

func (s *Server) texts(rec *Record) []map[string]any {
	var out []map[string]any
	for _, f := range rec.Files {
		if !f.IsText() || f.Filename == breadcrumbsFilename {
			continue // breadcrumbs get their own timeline
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
		out = append(out, map[string]any{"File": f, "Text": string(data), "Truncated": truncated})
	}
	return out
}

func (s *Server) handleUpdateIssue(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	issue, ok := s.issueFrom(w, r)
	if !ok {
		return
	}
	before := issue.Priority
	original := issue
	updated, err := s.issues.Update(p.Slug, issue.Number, func(i *Issue) error {
		if v := r.FormValue("status"); v != "" {
			if err := SetStatus(i, v); err != nil {
				return err
			}
		}
		if r.Form.Has("priority") {
			if err := SetPriority(i, r.FormValue("priority")); err != nil {
				return err
			}
		}
		if r.Form.Has("fixedIn") {
			SetFixedIn(i, r.FormValue("fixedIn"))
		}
		if r.Form.Has("title") {
			if err := SetTitle(i, r.FormValue("title")); err != nil {
				return err
			}
		}
		for _, l := range strings.Split(r.FormValue("addLabel"), ",") {
			AddLabel(i, l)
		}
		if l := r.FormValue("removeLabel"); l != "" {
			RemoveLabel(i, l)
		}
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.pushDiff(original, updated, "")
	if PriorityRank(updated.Priority) >= PriorityRank("high") && PriorityRank(updated.Priority) > PriorityRank(before) {
		s.notifier.Notify(p, updated.Kind, Alert{
			Event: EventPriority, Title: fmt.Sprintf("%s #%d is %s", p.Name, updated.Number, updated.Priority),
			Message: updated.Title, URL: s.issueURL(p.Slug, updated.Number), Urgent: updated.Priority == "urgent",
		})
	}
	s.back(w, r, issuePath(p.Slug, issue.Number))
}

func (s *Server) handleAddNote(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	issue, ok := s.issueFrom(w, r)
	if !ok {
		return
	}
	if _, err := s.issues.Update(p.Slug, issue.Number, func(i *Issue) error { return AddNote(i, r.FormValue("text")) }); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, issuePath(p.Slug, issue.Number)+"#notes", http.StatusSeeOther)
}

func (s *Server) handleDeleteNote(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	issue, ok := s.issueFrom(w, r)
	if !ok {
		return
	}
	_, _ = s.issues.Update(p.Slug, issue.Number, func(i *Issue) error { DeleteNote(i, r.PathValue("id")); return nil })
	http.Redirect(w, r, issuePath(p.Slug, issue.Number)+"#notes", http.StatusSeeOther)
}

func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	issue, ok := s.issueFrom(w, r)
	if !ok {
		return
	}
	// "into" merges this issue into another; "from" merges another into this one.
	if into, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(r.FormValue("into")), "#")); err == nil {
		if _, err := s.issues.Merge(p.Slug, into, []int{issue.Number}); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.pushMerged(p.Slug, into, []int{issue.Number})
		http.Redirect(w, r, issuePath(p.Slug, into), http.StatusSeeOther)
		return
	}
	from, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(r.FormValue("from")), "#"))
	if err != nil {
		http.Error(w, "enter an issue number", http.StatusBadRequest)
		return
	}
	if _, err := s.issues.Merge(p.Slug, issue.Number, []int{from}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.pushMerged(p.Slug, issue.Number, []int{from})
	http.Redirect(w, r, issuePath(p.Slug, issue.Number), http.StatusSeeOther)
}

func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	issue, ok := s.issueFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.SMTP == nil {
		http.Error(w, "Replies need SMTP. Set SNAG_SMTP_HOST and SNAG_SMTP_FROM.", http.StatusBadRequest)
		return
	}
	rec, err := s.store.Get(r.FormValue("report"))
	if err != nil || rec.Project != p.Slug || !contains(issue.Reports, rec.ID) {
		http.Error(w, "unknown report", http.StatusBadRequest)
		return
	}
	smtp := *s.cfg.SMTP
	smtp.ReplyTo = s.replyAddress(rec.ID)
	msg, err := BuildReply(smtp, rec.Contact, r.FormValue("subject"), r.FormValue("body"), rec.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := SendReply(ctx, *s.cfg.SMTP, rec.Contact, msg); err != nil {
		s.log.Warn("reply failed", "issue", issue.Number, "err", err)
		http.Redirect(w, r, issuePath(p.Slug, issue.Number)+"?flash="+url.QueryEscape("Couldn't send the reply: "+err.Error()), http.StatusSeeOther)
		return
	}
	_, _ = s.issues.Update(p.Slug, issue.Number, func(i *Issue) error {
		// Part of the conversation, so the reporter also sees it in the app and replies thread.
		if AddThreadMessage(i, FromTeam, rec.ID, r.FormValue("body")) == nil {
			i.Thread[len(i.Thread)-1].Via = "email"
			i.Activity = i.Activity[:len(i.Activity)-1]
		}
		i.log("reply", "Emailed the reporter (%s): %s", rec.Contact, truncate(strings.TrimSpace(r.FormValue("body")), 500))
		return nil
	})
	http.Redirect(w, r, issuePath(p.Slug, issue.Number)+"?flash="+url.QueryEscape("Reply sent to "+rec.Contact), http.StatusSeeOther)
}

func (s *Server) handleRetryForward(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	issue, ok := s.issueFrom(w, r)
	if !ok {
		return
	}
	if len(issue.Reports) > 0 {
		// Forwarding belongs to the report that opened the issue.
		_ = s.forwarder.Retry(issue.Reports[0])
	}
	http.Redirect(w, r, issuePath(p.Slug, issue.Number), http.StatusSeeOther)
}

func (s *Server) deleteIssue(project string, n int) {
	issue, err := s.issues.Get(project, n)
	if err != nil {
		return
	}
	for _, id := range issue.Reports {
		_ = s.store.Delete(id)
	}
	_ = s.issues.Delete(project, n)
}

func (s *Server) handleDeleteIssue(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	issue, ok := s.issueFrom(w, r)
	if !ok {
		return
	}
	s.deleteIssue(p.Slug, issue.Number)
	http.Redirect(w, r, "/p/"+p.Slug, http.StatusSeeOther)
}

func (s *Server) handleDeleteReport(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	rec, err := s.store.Get(r.PathValue("id"))
	if err != nil || rec.Project != p.Slug {
		http.NotFound(w, r)
		return
	}
	_ = s.store.Delete(rec.ID)
	_ = s.issues.Unfile(p.Slug, rec.Issue, rec.ID)
	if _, err := s.issues.Get(p.Slug, rec.Issue); err != nil {
		http.Redirect(w, r, "/p/"+p.Slug, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, issuePath(p.Slug, rec.Issue), http.StatusSeeOther)
}

func (s *Server) handleReportRedirect(w http.ResponseWriter, r *http.Request) {
	rec, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, issuePath(rec.Project, rec.Issue)+"#report-"+rec.ID, http.StatusSeeOther)
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
	if file.IsImage() || file.IsVideo() || (file.IsText() && r.URL.Query().Get("download") == "") {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", file.ContentType)
	w.Header().Set("Content-Disposition", disposition+`; filename="`+file.Filename+`"`)
	http.ServeFile(w, r, path)
}

// MARK: Releases

type versionRow struct {
	Version   string
	Reports   int
	Issues    int
	Open      int
	New       []*Issue // first reported in this version
	Fixed     []*Issue // fixed in this version
	Regressed []*Issue // regressed in this version
	FirstSeen time.Time
	LastSeen  time.Time
}

type dayBar struct {
	Day     time.Time
	Count   int
	Percent int
}

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	records, _ := s.store.List(Filter{Project: p.Slug})
	allIssues, _ := s.issues.List(p.Slug, IssueFilter{})
	issueByNumber := map[int]*Issue{}
	for _, issue := range allIssues {
		issueByNumber[issue.Number] = issue
	}
	rows := map[string]*versionRow{}
	issueSets := map[string]map[int]bool{}
	row := func(v string) *versionRow {
		if v == "" {
			v = "unknown"
		}
		if rows[v] == nil {
			rows[v] = &versionRow{Version: v}
			issueSets[v] = map[int]bool{}
		}
		return rows[v]
	}
	const days = 30
	today := time.Now().UTC().Truncate(24 * time.Hour)
	counts := make([]int, days)
	for _, rec := range records {
		v := reportVersion(rec)
		vr := row(v)
		vr.Reports++
		if vr.FirstSeen.IsZero() || rec.ReceivedAt.Before(vr.FirstSeen) {
			vr.FirstSeen = rec.ReceivedAt
		}
		if rec.ReceivedAt.After(vr.LastSeen) {
			vr.LastSeen = rec.ReceivedAt
		}
		issue := rec.Issue
		if i, ok := issueByNumber[issue]; ok && i.MergedInto != 0 {
			issue = i.MergedInto
		}
		if i, ok := issueByNumber[issue]; ok && !issueSets[vr.Version][issue] {
			issueSets[vr.Version][issue] = true
			vr.Issues++
			if i.IsOpen() {
				vr.Open++
			}
		}
		if d := int(today.Sub(rec.ReceivedAt.UTC().Truncate(24*time.Hour)).Hours() / 24); d >= 0 && d < days {
			counts[days-1-d]++
		}
	}
	for _, issue := range allIssues {
		if issue.FirstVersion != "" {
			vr := row(issue.FirstVersion)
			vr.New = append(vr.New, issue)
		}
		if issue.FixedIn != "" {
			vr := row(issue.FixedIn)
			vr.Fixed = append(vr.Fixed, issue)
		}
		if issue.Regressions > 0 && len(issue.AppVersions) > 0 {
			vr := row(issue.AppVersions[len(issue.AppVersions)-1])
			vr.Regressed = append(vr.Regressed, issue)
		}
	}
	list := make([]*versionRow, 0, len(rows))
	for _, vr := range rows {
		list = append(list, vr)
	}
	sort.Slice(list, func(i, j int) bool { return CompareVersions(list[i].Version, list[j].Version) > 0 })
	peak := 1
	for _, c := range counts {
		peak = max(peak, c)
	}
	bars := make([]dayBar, days)
	for i, c := range counts {
		bars[i] = dayBar{Day: today.AddDate(0, 0, i-days+1), Count: c, Percent: c * 100 / peak}
	}
	s.render(w, "releases.html", map[string]any{"P": p, "Rows": list, "Bars": bars, "Total": len(records), "Peak": peak})
}

// MARK: Settings

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, projectFrom(r), "", r.URL.Query().Get("flash"))
}

func (s *Server) renderSettings(w http.ResponseWriter, p *Project, newKey, flash string) {
	s.renderSettingsPage(w, p, newKey, "", flash)
}

func (s *Server) renderSettingsKey(w http.ResponseWriter, p *Project, newKey, scope string) {
	s.renderSettingsPage(w, p, newKey, scope, "")
}

func (s *Server) renderSettingsPage(w http.ResponseWriter, p *Project, newKey, scope, flash string) {
	gh := p.Forwarding.GitHub
	if gh == nil {
		gh = &ProjectGitHub{}
	}
	wh := p.Forwarding.Webhook
	if wh == nil {
		wh = &WebhookConfig{}
	}
	s.render(w, "settings.html", map[string]any{
		"P": p, "GH": gh, "WH": wh, "NewKey": newKey, "NewKeyScope": scope, "Flash": flash,
		"Symbols":        s.symbols.List(p.Slug),
		"HasServerToken": s.cfg.GitHubToken != "", "HasSMTP": s.cfg.SMTP != nil,
		"Ingest": s.cfg.PublicURL, "PerHour": s.cfg.NotifyPerHour, "HasWebhook": s.cfg.GitHubWebhookSecret != "",
	})
}

// secretField keeps the current value when the input is blank, and clears it for "-".
func secretField(current, input string) string {
	input = strings.TrimSpace(input)
	switch input {
	case "":
		return current
	case "-":
		return ""
	}
	return input
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	_, err := s.projects.Update(p.Slug, func(p *Project) error {
		if name := strings.TrimSpace(r.FormValue("name")); name != "" {
			p.Name = truncate(name, 80)
		}
		p.GroupDuplicates = r.FormValue("groupDuplicates") == "on"
		switch level := r.FormValue("redaction"); level {
		case RedactStandard, RedactStrict, RedactOff:
			p.Redaction = level
		}
		f := &p.Forwarding
		if repo := strings.TrimSpace(r.FormValue("githubRepo")); repo != "" {
			if owner, name, ok := strings.Cut(repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
				return errors.New("GitHub repository must be owner/repo")
			}
			old := f.GitHub
			if old == nil {
				old = &ProjectGitHub{}
			}
			f.GitHub = &ProjectGitHub{
				Repo:             repo,
				Labels:           splitList(r.FormValue("githubLabels")),
				AttachmentBranch: strings.TrimSpace(r.FormValue("githubBranch")),
				Token:            secretField(old.Token, r.FormValue("githubToken")),
			}
		} else {
			f.GitHub = nil
		}
		f.Discord = secretField(f.Discord, r.FormValue("discord"))
		f.Slack = secretField(f.Slack, r.FormValue("slack"))
		if hook := strings.TrimSpace(r.FormValue("webhookURL")); hook != "" {
			if u, err := url.Parse(hook); err != nil || (u.Scheme != "https" && u.Scheme != "http") {
				return errors.New("the webhook URL must be http(s)")
			}
			old := ""
			if f.Webhook != nil {
				old = f.Webhook.Secret
			}
			f.Webhook = &WebhookConfig{URL: hook, Secret: secretField(old, r.FormValue("webhookSecret"))}
		} else {
			f.Webhook = nil
		}
		f.Email = splitList(r.FormValue("email"))
		n := &p.Notify
		n.Ntfy = secretField(n.Ntfy, r.FormValue("ntfy"))
		n.NtfyToken = secretField(n.NtfyToken, r.FormValue("ntfyToken"))
		n.PushoverUser = secretField(n.PushoverUser, r.FormValue("pushoverUser"))
		n.PushoverToken = secretField(n.PushoverToken, r.FormValue("pushoverToken"))
		n.OnNewIssue = r.FormValue("onNewIssue") == "on"
		n.OnRegression = r.FormValue("onRegression") == "on"
		n.OnReport = r.FormValue("onReport") == "on"
		n.OnPriority = r.FormValue("onPriority") == "on"
		n.Kinds = splitList(r.FormValue("notifyKinds"))
		switch mode := r.FormValue("attestMode"); mode {
		case AttestOff, AttestMonitor, AttestRequire:
			p.Attest.Mode = mode
		default:
			return errors.New("unknown App Attest mode")
		}
		p.Attest.AppIDs = splitList(r.FormValue("attestAppIDs"))
		p.Attest.AllowDevelopment = r.FormValue("attestDevelopment") == "on"
		for _, id := range p.Attest.AppIDs {
			if team, bundle, ok := strings.Cut(id, "."); !ok || len(team) != 10 || bundle == "" {
				return fmt.Errorf("%q isn't an app ID; use TEAMID.bundle.id", id)
			}
		}
		if p.Attest.Mode != AttestOff && len(p.Attest.AppIDs) == 0 {
			return errors.New("App Attest needs at least one app ID")
		}
		if n.Ntfy != "" {
			if u, err := url.Parse(n.Ntfy); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return errors.New("the ntfy topic must be a URL like https://ntfy.sh/your-topic")
			}
		}
		for _, hook := range []string{f.Discord, f.Slack} {
			if hook != "" && !strings.HasPrefix(hook, "https://") {
				return errors.New("Discord and Slack webhooks must be https URLs")
			}
		}
		return nil
	})
	if err != nil {
		p.Name = r.FormValue("name")
		s.renderSettings(w, p, "", "Not saved: "+err.Error())
		return
	}
	http.Redirect(w, r, "/p/"+p.Slug+"/settings?flash=Saved", http.StatusSeeOther)
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	key, err := s.projects.AddKey(p.Slug, r.FormValue("label"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p, _ = s.projects.Get(p.Slug)
	s.renderSettings(w, p, key, "")
}

func (s *Server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	if !p.Notify.Enabled() {
		http.Redirect(w, r, "/p/"+p.Slug+"/settings?flash="+url.QueryEscape("Set up ntfy or Pushover first"), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err := s.notifier.Send(ctx, p.Notify, Alert{Event: EventTest, Title: p.Name + " · test notification",
		Message: "Notifications from Snag are working.", URL: s.cfg.PublicURL + "/p/" + p.Slug})
	flash := "Test notification sent"
	if err != nil {
		flash = "Test notification failed: " + err.Error()
	}
	http.Redirect(w, r, "/p/"+p.Slug+"/settings?flash="+url.QueryEscape(flash), http.StatusSeeOther)
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	_ = s.projects.RevokeKey(p.Slug, r.PathValue("id"))
	http.Redirect(w, r, "/p/"+p.Slug+"/settings?flash=Key+revoked", http.StatusSeeOther)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	if r.FormValue("confirm") != p.Slug {
		http.Redirect(w, r, "/p/"+p.Slug+"/settings?flash="+url.QueryEscape("Type the slug to confirm deletion"), http.StatusSeeOther)
		return
	}
	if err := s.projects.Delete(p.Slug); err != nil {
		http.Redirect(w, r, "/p/"+p.Slug+"/settings?flash="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	records, _ := s.store.List(Filter{Project: p.Slug})
	for _, rec := range records {
		_ = s.store.Delete(rec.ID)
	}
	_ = s.issues.DeleteProject(p.Slug)
	_ = s.symbols.DeleteProject(p.Slug)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// safeReturn only allows local paths, so a form can't redirect off-site.
func safeReturn(target, fallback string) string {
	if strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") && !strings.Contains(target, "\\") {
		return target
	}
	return fallback
}
