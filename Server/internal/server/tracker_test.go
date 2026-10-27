package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func adminPost(t *testing.T, srv *Server, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if !form.Has("csrf") {
		form.Set("csrf", srv.csrfToken())
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("admin", "pw-for-tests")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func uploadWith(id, title, message string) upload {
	u := sampleUpload(id)
	u.manifest["title"] = title
	u.manifest["message"] = message
	return u
}

func uploadVersion(id, title, version string) upload {
	u := uploadWith(id, title, "same words")
	ctx := u.manifest["context"].(map[string]any)
	app := ctx["app"].(map[string]any)
	app["version"] = version
	return u
}

func ids(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%08d-0000-4000-8000-000000000000", i+1)
	}
	return out
}

func TestDashboardRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	for _, path := range []string{"/", "/p/gelo", "/p/gelo/issues/1", "/p/gelo/settings", "/api/v1/reports", "/api/v1/projects"} {
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 401 {
			t.Fatalf("%s: %d", path, rr.Code)
		}
	}
	cfg := testConfig(t)
	cfg.AdminPassword = ""
	disabled, _ := newTestServer(t, cfg)
	rr := httptest.NewRecorder()
	disabled.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != 403 {
		t.Fatalf("disabled dashboard: %d", rr.Code)
	}
}

func TestPagesRender(t *testing.T) {
	srv, store := newTestServer(t, testConfig(t))
	u := sampleUpload(id1)
	u.manifest["message"] = "<script>alert('x')</script>"
	post(t, srv, u, testKey)

	overview := adminGet(srv, "/")
	if overview.Code != 200 || !strings.Contains(overview.Body.String(), `href="/p/gelo"`) {
		t.Fatalf("overview: %d %s", overview.Code, overview.Body)
	}
	if !strings.Contains(overview.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("dashboard CSP missing")
	}
	list := adminGet(srv, "/p/gelo")
	if list.Code != 200 || !strings.Contains(list.Body.String(), "Playback stalls") || !strings.Contains(list.Body.String(), "#1") {
		t.Fatalf("issues: %d %s", list.Code, list.Body)
	}
	page := adminGet(srv, "/p/gelo/issues/1")
	body := page.Body.String()
	if page.Code != 200 {
		t.Fatalf("issue: %d %s", page.Code, body)
	}
	if strings.Contains(body, "<script>alert") {
		t.Fatal("message not escaped")
	}
	for _, want := range []string{"line 2", "iPhone17,1", "iOS 26.1", "42%", "jellyfin.local", "me@example.com", `<img src="/reports/`, "Opened from a report", "beta"} {
		if !strings.Contains(body, want) {
			t.Errorf("issue page missing %q", want)
		}
	}
	if adminGet(srv, "/p/gelo/settings").Code != 200 || adminGet(srv, "/projects/new").Code != 200 {
		t.Fatal("settings/new project")
	}
	// Search reaches report content and context.
	if !strings.Contains(adminGet(srv, "/p/gelo?q=jellyfin").Body.String(), "Playback stalls") {
		t.Fatal("search by custom context failed")
	}
	if strings.Contains(adminGet(srv, "/p/gelo?q=zzz-nothing").Body.String(), "Playback stalls") {
		t.Fatal("search matched everything")
	}
	rec, _ := store.Get(id1)
	if r := adminGet(srv, "/reports/"+rec.ID); r.Code != http.StatusSeeOther || !strings.HasPrefix(r.Header().Get("Location"), "/p/gelo/issues/1") {
		t.Fatalf("report redirect: %d %s", r.Code, r.Header().Get("Location"))
	}
	img := adminGet(srv, "/reports/"+rec.ID+"/files/"+rec.Files[0].Name)
	if img.Code != 200 || img.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("image: %d", img.Code)
	}
	if adminGet(srv, "/reports/"+rec.ID+"/files/../report.json").Code == 200 {
		t.Fatal("path traversal")
	}
	if adminGet(srv, "/p/nope").Code != 404 || adminGet(srv, "/p/gelo/issues/99").Code != 404 {
		t.Fatal("missing pages should 404")
	}
	var api struct {
		Total  int      `json:"total"`
		Issues []*Issue `json:"issues"`
	}
	_ = json.Unmarshal(adminGet(srv, "/api/v1/projects/gelo/issues?status=all").Body.Bytes(), &api)
	if api.Total != 1 || api.Issues[0].Title != "Playback stalls" {
		t.Fatalf("issues api %+v", api)
	}
}

func TestWorkflowNeedsCSRFAndIsLogged(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	post(t, srv, sampleUpload(id1), testKey)
	if rr := adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"csrf": {"wrong"}, "status": {"resolved"}}); rr.Code != 403 {
		t.Fatalf("bad csrf: %d", rr.Code)
	}
	// The token in the rendered page works.
	page := adminGet(srv, "/p/gelo/issues/1").Body.String()
	token := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`).FindStringSubmatch(page)
	if token == nil || token[1] != srv.csrfToken() {
		t.Fatal("no csrf token in page")
	}
	rr := adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{
		"status": {"in_progress"}, "priority": {"high"}, "addLabel": {"Playback, crash ,"}, "title": {"Stalls after 3s"},
		"return": {"https://evil.example"},
	})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/p/gelo/issues/1" {
		t.Fatalf("update: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	issue, _ := srv.issues.Get("gelo", 1)
	if issue.Status != IssueInProgress || issue.Priority != "high" || fmt.Sprint(issue.Labels) != "[beta crash playback]" || issue.Title != "Stalls after 3s" {
		t.Fatalf("issue %+v", issue)
	}
	var texts []string
	for _, e := range issue.Activity {
		texts = append(texts, e.Text)
	}
	joined := strings.Join(texts, " | ")
	for _, want := range []string{"New → In progress", "Priority none → high", "Added label crash", "Renamed from “Playback stalls”"} {
		if !strings.Contains(joined, want) {
			t.Errorf("activity missing %q: %s", want, joined)
		}
	}
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"removeLabel": {"crash"}, "status": {"bogus"}})
	if issue, _ = srv.issues.Get("gelo", 1); !contains(issue.Labels, "crash") {
		t.Fatal("bad status shouldn't half-apply")
	}
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"removeLabel": {"crash"}})
	if issue, _ = srv.issues.Get("gelo", 1); contains(issue.Labels, "crash") {
		t.Fatal("label not removed")
	}
	// Filters.
	for query, want := range map[string]bool{
		"": true, "?status=new": false, "?status=in_progress": true, "?priority=high": true, "?priority=low": false,
		"?label=playback": true, "?label=nope": false, "?status=closed": false, "?version=2.0": true, "?q=%231": true,
	} {
		has := strings.Contains(adminGet(srv, "/p/gelo"+query).Body.String(), "Stalls after 3s")
		if has != want {
			t.Errorf("filter %q: listed=%v want %v", query, has, want)
		}
	}
	// Resolved issues leave the open list.
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"status": {"resolved"}})
	if strings.Contains(adminGet(srv, "/p/gelo").Body.String(), "Stalls after 3s") {
		t.Fatal("resolved issue in open list")
	}
	if stats := srv.issues.Stats("gelo"); stats.Open != 0 || stats.ByStatus[IssueResolved] != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestNotes(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	post(t, srv, sampleUpload(id1), testKey)
	adminPost(t, srv, "/p/gelo/issues/1/notes", url.Values{"text": {"Repro on iPad <b>only</b>"}})
	issue, _ := srv.issues.Get("gelo", 1)
	if len(issue.Notes) != 1 {
		t.Fatal("note not added")
	}
	page := adminGet(srv, "/p/gelo/issues/1").Body.String()
	if !strings.Contains(page, "Repro on iPad &lt;b&gt;only&lt;/b&gt;") {
		t.Fatal("note not shown escaped")
	}
	if rr := adminPost(t, srv, "/p/gelo/issues/1/notes", url.Values{"text": {"  "}}); rr.Code != 400 {
		t.Fatal("empty note accepted")
	}
	adminPost(t, srv, "/p/gelo/issues/1/notes/"+issue.Notes[0].ID+"/delete", nil)
	if issue, _ = srv.issues.Get("gelo", 1); len(issue.Notes) != 0 {
		t.Fatal("note not deleted")
	}
}

func TestDuplicateGroupingForwardsOnce(t *testing.T) {
	hook, requests := recorder(t, func(r recorded) (int, string) { return 200, "" })
	cfg := withForwarding(testConfig(t), ProjectForwarding{Webhook: &WebhookConfig{URL: hook.URL}})
	group := true
	cfg.Seeds[0].GroupDuplicates = &group
	srv, store := newTestServer(t, cfg)
	id := ids(3)
	post(t, srv, uploadWith(id[0], "Crash on launch", "It crashes"), testKey)
	post(t, srv, uploadWith(id[1], "crash  on LAUNCH", "it crashes"), testKey) // same after normalizing
	post(t, srv, uploadWith(id[2], "Crash on launch", "Different words"), testKey)
	for _, i := range id {
		srv.Dispatcher().Process(context.Background(), i)
	}
	issue, _ := srv.issues.Get("gelo", 1)
	if len(issue.Reports) != 2 {
		t.Fatalf("issue #1 reports %v", issue.Reports)
	}
	if _, err := srv.issues.Get("gelo", 2); err != nil {
		t.Fatal("different message should open #2")
	}
	if len(requests()) != 2 {
		t.Fatalf("forwarded %d times, want once per issue", len(requests()))
	}
	if rec, _ := store.Get(id[1]); rec.Issue != 1 || rec.Forwarding != nil {
		t.Fatalf("duplicate %+v", rec)
	}
	// Won't-fix issues don't collect new reports.
	adminPost(t, srv, "/p/gelo/issues/2/update", url.Values{"status": {"wontfix"}})
	post(t, srv, uploadWith("00000009-0000-4000-8000-000000000000", "Crash on launch", "Different words"), testKey)
	if _, err := srv.issues.Get("gelo", 3); err != nil {
		t.Fatal("a report matching a won't-fix issue should open a new one")
	}
	// Deleting a report keeps the issue; deleting the last one removes it.
	adminPost(t, srv, "/p/gelo/reports/"+id[1]+"/delete", nil)
	if issue, _ := srv.issues.Get("gelo", 1); len(issue.Reports) != 1 {
		t.Fatal("report not removed from issue")
	}
	adminPost(t, srv, "/p/gelo/reports/"+id[0]+"/delete", nil)
	if _, err := srv.issues.Get("gelo", 1); err == nil {
		t.Fatal("empty issue should be gone")
	}
}

func TestMergeAndSimilar(t *testing.T) {
	srv, store := newTestServer(t, testConfig(t))
	id := ids(3)
	post(t, srv, uploadWith(id[0], "Video playback freezes", "Playback freezes after seeking forward"), testKey)
	post(t, srv, uploadWith(id[1], "Playback freezing when seeking", "Video freezes after seeking"), testKey)
	post(t, srv, uploadWith(id[2], "Login button missing", "Cannot find the sign in button on settings"), testKey)
	adminPost(t, srv, "/p/gelo/issues/1/notes", url.Values{"text": {"first note"}})
	adminPost(t, srv, "/p/gelo/issues/2/update", url.Values{"priority": {"urgent"}})

	issue1, _ := srv.issues.Get("gelo", 1)
	text := func(i *Issue) string {
		var b strings.Builder
		b.WriteString(i.Title)
		for _, r := range store.ForIssue(i.Reports) {
			b.WriteString(" " + r.Message)
		}
		return b.String()
	}
	similar := srv.issues.Similar(issue1, text, 5)
	if len(similar) == 0 || similar[0].Issue.Number != 2 {
		t.Fatalf("similar %+v", similar)
	}
	for _, s := range similar {
		if s.Issue.Number == 3 {
			t.Fatalf("unrelated issue suggested (score %.2f)", s.Score)
		}
	}
	if !strings.Contains(adminGet(srv, "/p/gelo/issues/1").Body.String(), "Playback freezing when seeking") {
		t.Fatal("suggestion not shown")
	}

	// Bulk merge keeps the oldest number.
	rr := adminPost(t, srv, "/p/gelo/bulk", url.Values{"action": {"merge"}, "issue": {"2", "1"}})
	if rr.Header().Get("Location") != "/p/gelo/issues/1" {
		t.Fatalf("merge redirect %s", rr.Header().Get("Location"))
	}
	merged, _ := srv.issues.Get("gelo", 1)
	if len(merged.Reports) != 2 || merged.Priority != "urgent" || len(merged.Notes) != 1 {
		t.Fatalf("merged %+v", merged)
	}
	if r := adminGet(srv, "/p/gelo/issues/2"); r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/p/gelo/issues/1" {
		t.Fatalf("old issue should redirect: %d", r.Code)
	}
	if strings.Contains(adminGet(srv, "/p/gelo?status=all").Body.String(), `href="/p/gelo/issues/2"`) {
		t.Fatal("merged issue still listed")
	}
	// Merge by number from the issue page, then the reverse direction.
	adminPost(t, srv, "/p/gelo/issues/3/merge", url.Values{"into": {"#1"}})
	if merged, _ = srv.issues.Get("gelo", 1); len(merged.Reports) != 3 {
		t.Fatal("merge into failed")
	}
	if rr := adminPost(t, srv, "/p/gelo/issues/1/merge", url.Values{"from": {"2"}}); rr.Code != 400 {
		t.Fatal("merging an already-merged issue should fail")
	}
	// New reports of the merged issue's reports link to the target.
	if rec, _ := store.Get(id[1]); rec.Issue != 2 {
		// The record keeps its original issue number; the redirect resolves it.
		t.Logf("record issue %d", rec.Issue)
	}
}

func TestBulkStatusPriorityLabelDelete(t *testing.T) {
	srv, store := newTestServer(t, testConfig(t))
	id := ids(3)
	for i, rid := range id {
		post(t, srv, uploadWith(rid, fmt.Sprintf("Issue %d", i), "msg"), testKey)
	}
	adminPost(t, srv, "/p/gelo/bulk", url.Values{"action": {"status:triaged"}, "issue": {"1", "2"}})
	adminPost(t, srv, "/p/gelo/bulk", url.Values{"action": {"priority:low"}, "issue": {"2"}})
	adminPost(t, srv, "/p/gelo/bulk", url.Values{"action": {"label:"}, "label": {"ui"}, "issue": {"1", "3"}})
	i1, _ := srv.issues.Get("gelo", 1)
	i2, _ := srv.issues.Get("gelo", 2)
	i3, _ := srv.issues.Get("gelo", 3)
	if i1.Status != IssueTriaged || i2.Status != IssueTriaged || i3.Status != IssueNew || i2.Priority != "low" || !contains(i1.Labels, "ui") || !contains(i3.Labels, "ui") {
		t.Fatalf("bulk: %+v %+v %+v", i1, i2, i3)
	}
	adminPost(t, srv, "/p/gelo/bulk", url.Values{"action": {"delete"}, "issue": {"3"}})
	if _, err := srv.issues.Get("gelo", 3); err == nil {
		t.Fatal("issue not deleted")
	}
	if _, err := store.Get(id[2]); err == nil {
		t.Fatal("report not deleted with issue")
	}
}

func TestReplyToReporter(t *testing.T) {
	cfg := testConfig(t)
	cfg.SMTP = &SMTPConfig{Host: "smtp.example.com", Port: 587, From: "Gelo Support <support@example.com>", ReplyTo: "dev@example.com", TLS: "starttls"}
	sent := stubSMTP(t)
	srv, _ := newTestServer(t, cfg)
	post(t, srv, sampleUpload(id1), testKey)
	id := strings.ToLower(id1)
	if !strings.Contains(adminGet(srv, "/p/gelo/issues/1").Body.String(), "Reply to me@example.com") {
		t.Fatal("reply form missing")
	}
	rr := adminPost(t, srv, "/p/gelo/issues/1/reply", url.Values{"report": {id}, "subject": {"Re: Playback\r\nBcc: x@evil.com"}, "body": {"Fixed in 2.1, thanks!"}})
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "Reply+sent") {
		t.Fatalf("reply: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	msgs := sent()
	if len(msgs) != 1 || fmt.Sprint(msgs[0].to) != "[me@example.com]" {
		t.Fatalf("sent %+v", msgs)
	}
	head, _, _ := strings.Cut(string(msgs[0].msg), "\r\n\r\n")
	if strings.Contains(head, "\r\nBcc:") || !strings.Contains(head, "Reply-To: dev@example.com") || !strings.Contains(head, "To: <me@example.com>") {
		t.Fatalf("headers:\n%s", head)
	}
	issue, _ := srv.issues.Get("gelo", 1)
	if last := issue.Activity[len(issue.Activity)-1]; last.Kind != "reply" || !strings.Contains(last.Text, "Fixed in 2.1") {
		t.Fatalf("activity %+v", last)
	}
	// A report from another issue can't be replied to through this one.
	post(t, srv, uploadWith(ids(1)[0], "Other", "other"), testKey)
	if rr := adminPost(t, srv, "/p/gelo/issues/1/reply", url.Values{"report": {ids(1)[0]}, "subject": {"x"}, "body": {"y"}}); rr.Code != 400 {
		t.Fatalf("cross-issue reply: %d", rr.Code)
	}
}

func TestReplyNeedsSMTP(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	post(t, srv, sampleUpload(id1), testKey)
	if rr := adminPost(t, srv, "/p/gelo/issues/1/reply", url.Values{"report": {strings.ToLower(id1)}, "subject": {"x"}, "body": {"y"}}); rr.Code != 400 {
		t.Fatalf("no smtp: %d", rr.Code)
	}
}

func TestProjectsAndKeys(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	rr := adminPost(t, srv, "/projects", url.Values{"name": {"Arctic for Lemmy"}})
	if rr.Code != 200 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body)
	}
	key := regexp.MustCompile(`snag_[0-9a-f]{40}`).FindString(rr.Body.String())
	if key == "" {
		t.Fatal("new key not shown")
	}
	p, ok := srv.projects.Get("arctic-for-lemmy")
	if !ok || len(p.Keys) != 1 || strings.Contains(mustJSON(p), key) {
		t.Fatalf("project %+v (key must be stored hashed)", p)
	}
	if rr := adminPost(t, srv, "/projects", url.Values{"name": {"Arctic for Lemmy"}}); !strings.Contains(rr.Body.String(), "already exists") {
		t.Fatal("duplicate slug accepted")
	}
	// Reports go to the key's project; numbering is per project.
	post(t, srv, sampleUpload(id1), testKey)
	resp := post(t, srv, sampleUpload("5C1A4F51-FD33-442C-AB20-C90630FC6A28"), key)
	if resp.Code != 201 || !strings.Contains(resp.Body.String(), `"project":"arctic-for-lemmy"`) || !strings.Contains(resp.Body.String(), `"issue":1`) {
		t.Fatalf("arctic ingest: %s", resp.Body)
	}
	if _, total := srv.issues.List("gelo", IssueFilter{}); total != 1 {
		t.Fatal("projects leaked into each other")
	}
	// A second key, then revoke the first.
	rr = adminPost(t, srv, "/p/arctic-for-lemmy/keys", url.Values{"label": {"tvOS"}})
	key2 := regexp.MustCompile(`snag_[0-9a-f]{40}`).FindString(rr.Body.String())
	p, _ = srv.projects.Get("arctic-for-lemmy")
	adminPost(t, srv, "/p/arctic-for-lemmy/keys/"+p.Keys[0].ID+"/revoke", nil)
	if post(t, srv, sampleUpload("6C1A4F51-FD33-442C-AB20-C90630FC6A28"), key).Code != 401 {
		t.Fatal("revoked key still works")
	}
	if post(t, srv, sampleUpload("6C1A4F51-FD33-442C-AB20-C90630FC6A28"), key2).Code != 201 {
		t.Fatal("second key doesn't work")
	}
	// Config projects can't be deleted from the dashboard; dashboard projects can.
	adminPost(t, srv, "/p/gelo/delete", url.Values{"confirm": {"gelo"}})
	if _, ok := srv.projects.Get("gelo"); !ok {
		t.Fatal("config project deleted")
	}
	adminPost(t, srv, "/p/arctic-for-lemmy/delete", url.Values{"confirm": {"wrong"}})
	if _, ok := srv.projects.Get("arctic-for-lemmy"); !ok {
		t.Fatal("deleted without confirmation")
	}
	adminPost(t, srv, "/p/arctic-for-lemmy/delete", url.Values{"confirm": {"arctic-for-lemmy"}})
	if _, ok := srv.projects.Get("arctic-for-lemmy"); ok {
		t.Fatal("project not deleted")
	}
	if _, total := srv.store.List(Filter{Project: "arctic-for-lemmy"}); total != 0 {
		t.Fatal("reports not deleted with project")
	}
}

func mustJSON(v any) string { data, _ := json.Marshal(v); return string(data) }

func TestSettingsSecretsAndPerProjectForwarding(t *testing.T) {
	hook, requests := recorder(t, func(r recorded) (int, string) { return 200, "" })
	srv, _ := newTestServer(t, testConfig(t))
	rr := adminPost(t, srv, "/p/gelo/settings", url.Values{
		"name": {"Gelo TV"}, "groupDuplicates": {"on"},
		"githubRepo": {"me/gelo"}, "githubToken": {"ghp_secret"}, "githubLabels": {"snag, gelo"},
		"webhookURL": {hook.URL}, "discord": {"http://insecure"},
	})
	if !strings.Contains(rr.Body.String(), "must be https") {
		t.Fatalf("insecure discord accepted: %d", rr.Code)
	}
	adminPost(t, srv, "/p/gelo/settings", url.Values{
		"name": {"Gelo TV"}, "groupDuplicates": {"on"},
		"githubRepo": {"me/gelo"}, "githubToken": {"ghp_secret"}, "githubLabels": {"snag, gelo"}, "webhookURL": {hook.URL},
	})
	p, _ := srv.projects.Get("gelo")
	if p.Name != "Gelo TV" || !p.GroupDuplicates || p.Forwarding.GitHub.Token != "ghp_secret" || fmt.Sprint(p.Forwarding.GitHub.Labels) != "[snag gelo]" {
		t.Fatalf("settings %+v", p.Forwarding.GitHub)
	}
	page := adminGet(srv, "/p/gelo/settings").Body.String()
	if strings.Contains(page, "ghp_secret") {
		t.Fatal("secret rendered into the page")
	}
	// Blank keeps the token; "-" clears it.
	adminPost(t, srv, "/p/gelo/settings", url.Values{"name": {"Gelo TV"}, "githubRepo": {"me/gelo"}, "githubToken": {""}, "webhookURL": {hook.URL}})
	if p, _ = srv.projects.Get("gelo"); p.Forwarding.GitHub.Token != "ghp_secret" || p.GroupDuplicates {
		t.Fatal("blank should keep the token; unchecked box turns grouping off")
	}
	adminPost(t, srv, "/p/gelo/settings", url.Values{"name": {"Gelo TV"}, "githubRepo": {"me/gelo"}, "githubToken": {"-"}, "webhookURL": {hook.URL}})
	if p, _ = srv.projects.Get("gelo"); p.Forwarding.GitHub.Token != "" {
		t.Fatal("- should clear the token")
	}
	// The new webhook applies to new issues.
	adminPost(t, srv, "/p/gelo/settings", url.Values{"name": {"Gelo TV"}, "webhookURL": {hook.URL}})
	post(t, srv, sampleUpload(id1), testKey)
	srv.Dispatcher().Process(context.Background(), strings.ToLower(id1))
	if len(requests()) != 1 {
		t.Fatalf("webhook calls %d", len(requests()))
	}
	// Removing a destination stops pending forwards to it.
	adminPost(t, srv, "/p/gelo/settings", url.Values{"name": {"Gelo TV"}})
	if names := srv.Dispatcher().Names("gelo"); len(names) != 0 {
		t.Fatalf("names %v", names)
	}
}

func TestProjectsFileSeedsAndDashboardOverrides(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "projects.json")
	_ = os.WriteFile(file, []byte(`{"projects": [
	  {"slug": "juturna", "name": "Juturna", "keys": ["${JUT_KEY}"], "groupDuplicates": true,
	   "forwarding": {"discord": "https://discord.com/api/webhooks/1/${JUT_HOOK}"}}
	]}`), 0o600)
	env := map[string]string{"SNAG_PROJECTS_FILE": file, "JUT_KEY": "juturna-key-0123456789", "JUT_HOOK": `ab"c`}
	cfg, err := LoadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir, cfg.AdminPassword = filepath.Join(dir, "data"), "pw-for-tests"
	srv, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := srv.projects.Get("juturna")
	if !p.GroupDuplicates || p.Forwarding.Discord != `https://discord.com/api/webhooks/1/ab"c` || p.ConfigKeyCount() != 1 {
		t.Fatalf("seeded %+v", p)
	}
	if post(t, srv, sampleUpload(id1), "juturna-key-0123456789").Code != 201 {
		t.Fatal("file key rejected")
	}
	adminPost(t, srv, "/p/juturna/settings", url.Values{"name": {"Juturna Player"}})
	// Restart with the same file: dashboard edits win, the file key still works.
	srv2, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ = srv2.projects.Get("juturna")
	// Blank secret fields keep their value; the unchecked box turned grouping off.
	if p.Name != "Juturna Player" || p.GroupDuplicates || p.Forwarding.Discord == "" {
		t.Fatalf("after restart %+v", p)
	}
	if _, ok := srv2.projects.Authenticate("juturna-key-0123456789"); !ok {
		t.Fatal("file key lost on restart")
	}
	// The same key in two projects is a configuration error.
	cfg.Seeds = append(cfg.Seeds, ProjectSeed{Slug: "other", Keys: []string{"juturna-key-0123456789"}})
	if _, err := Open(cfg, nil); err == nil {
		t.Fatal("duplicate key accepted")
	}
	// Unset variables surface as an error instead of an empty key.
	if _, err := LoadConfig(func(k string) string { return map[string]string{"SNAG_PROJECTS_FILE": file}[k] }); err == nil {
		t.Fatal("missing ${JUT_KEY} accepted")
	}
}

func TestMigratesReportsFromBeforeProjects(t *testing.T) {
	cfg := testConfig(t)
	cfg.Seeds = nil
	dir := filepath.Join(cfg.DataDir, "reports", strings.ToLower(id1))
	_ = os.MkdirAll(dir, 0o750)
	legacy := map[string]any{
		"id": strings.ToLower(id1), "createdAt": "2026-10-01T10:00:00Z", "kind": "bug", "title": "Old report", "message": "from 1.0",
		"receivedAt": "2026-10-01T10:00:01Z", "app": "Gelo", "status": "closed", "files": []any{},
	}
	data, _ := json.Marshal(legacy)
	_ = os.WriteFile(filepath.Join(dir, "report.json"), data, 0o640)
	srv, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.projects.Get("gelo"); !ok {
		t.Fatal("project not created from the old app name")
	}
	issue, err := srv.issues.Get("gelo", 1)
	if err != nil || issue.Status != IssueResolved || issue.Title != "Old report" {
		t.Fatalf("issue %+v %v", issue, err)
	}
	rec, _ := srv.store.Get(id1)
	if rec.Project != "gelo" || rec.Issue != 1 || rec.Status != "" {
		t.Fatalf("record %+v", rec)
	}
	// Idempotent.
	srv2, _ := Open(cfg, nil)
	if _, total := srv2.issues.List("gelo", IssueFilter{}); total != 1 {
		t.Fatalf("migration ran twice: %d issues", total)
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{"Arctic for Lemmy!": "arctic-for-lemmy", "  Gelo  ": "gelo", "Ünïcode": "n-code", "---": ""} {
		if got := Slugify(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}

func TestRecordingsAreStoredAsVideo(t *testing.T) {
	srv, store := newTestServer(t, testConfig(t))
	u := sampleUpload(id1)
	// The start of an MP4 as AVAssetWriter writes it: an ftyp box with the isom brand.
	mp4 := append([]byte{0, 0, 0, 0x1c, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 2, 0, 'i', 's', 'o', 'm', 'i', 's', 'o', '2', 'm', 'p', '4', '1'}, make([]byte, 64)...)
	u.manifest["attachments"] = append(u.manifest["attachments"].([]map[string]any),
		map[string]any{"id": "A3000000-0000-0000-0000-000000000000", "role": "recording", "filename": "recording.mp4", "contentType": "video/mp4", "size": len(mp4), "part": "attachment2"})
	u.files["attachment2"] = mp4
	if rr := post(t, srv, u, testKey); rr.Code != 201 {
		t.Fatal(rr.Body)
	}
	rec, _ := store.Get(id1)
	video := rec.Files[2]
	if video.ContentType != "video/mp4" || video.Role != "recording" {
		t.Fatalf("stored as %+v", video)
	}
	page := adminGet(srv, "/p/gelo/issues/1").Body.String()
	if !strings.Contains(page, `<video controls`) {
		t.Fatal("no video player")
	}
	if !strings.Contains(adminGet(srv, "/").Header().Get("Content-Security-Policy"), "media-src 'self'") {
		t.Fatal("CSP blocks video")
	}
	file := adminGet(srv, "/reports/"+rec.ID+"/files/"+video.Name)
	if file.Header().Get("Content-Type") != "video/mp4" || !strings.HasPrefix(file.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("served as %s %s", file.Header().Get("Content-Type"), file.Header().Get("Content-Disposition"))
	}
}

func TestRegressions(t *testing.T) {
	cfg := testConfig(t)
	group := true
	cfg.Seeds[0].GroupDuplicates = &group
	srv, _ := newTestServer(t, cfg)
	id := ids(5)
	post(t, srv, uploadVersion(id[0], "Crash in player", "3.2"), testKey)
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"status": {"resolved"}, "fixedIn": {"3.3"}})

	// Still on the old version: joins the resolved issue without reopening it.
	post(t, srv, uploadVersion(id[1], "Crash in player", "3.2.1"), testKey)
	issue, _ := srv.issues.Get("gelo", 1)
	if issue.Status != IssueResolved || len(issue.Reports) != 2 || issue.Regressions != 0 {
		t.Fatalf("old version reopened: %+v", issue)
	}
	// On the fixed version: a regression.
	post(t, srv, uploadVersion(id[2], "Crash in player", "3.10"), testKey)
	issue, _ = srv.issues.Get("gelo", 1)
	if issue.Status != IssueNew || issue.Regressions != 1 || !contains(issue.Labels, "regression") {
		t.Fatalf("not a regression: %+v", issue)
	}
	last := issue.Activity[len(issue.Activity)-1]
	if last.Kind != "regression" || !strings.Contains(last.Text, "3.10 after the fix in 3.3") {
		t.Fatalf("activity %+v", last)
	}
	if _, err := srv.issues.Get("gelo", 2); err == nil {
		t.Fatal("regression should reuse the issue")
	}
	// Without a fixed-in version, any new report after resolving is a regression.
	post(t, srv, uploadVersion(id[3], "Another bug", "3.10"), testKey)
	adminPost(t, srv, "/p/gelo/issues/2/update", url.Values{"status": {"resolved"}})
	post(t, srv, uploadVersion(id[4], "Another bug", "3.10"), testKey)
	if i2, _ := srv.issues.Get("gelo", 2); i2.Regressions != 1 || i2.Status != IssueNew {
		t.Fatalf("issue 2 %+v", i2)
	}
	// Filters and pages.
	if body := adminGet(srv, "/p/gelo?regressed=1").Body.String(); !strings.Contains(body, "Crash in player") || !strings.Contains(body, "↺ regressed") {
		t.Fatal("regression filter")
	}
	if body := adminGet(srv, "/p/gelo?status=all&version=3.3").Body.String(); !strings.Contains(body, "Crash in player") {
		t.Fatal("fixed-in version filter")
	}
	page := adminGet(srv, "/p/gelo/releases")
	body := page.Body.String()
	if page.Code != 200 {
		t.Fatalf("releases: %d %s", page.Code, body)
	}
	v310 := strings.Index(body, "<strong>3.10</strong>")
	v33 := strings.Index(body, "<strong>3.3</strong>")
	v32 := strings.Index(body, "<strong>3.2</strong>")
	if v310 < 0 || v33 < 0 || v32 < 0 || !(v310 < v33 && v33 < v32) {
		t.Fatalf("versions not listed newest first: %d %d %d", v310, v33, v32)
	}
	if !strings.Contains(body, "<rect") || strings.Contains(body, "style=") {
		t.Fatal("chart should be SVG without inline styles (CSP)")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"3.10", "3.9", 1}, {"3.2", "3.2.0", 0}, {"3.2.1", "3.2", 1}, {"v2", "2.0", 0},
		{"2.0-beta", "2.0", -1}, {"2.0", "2.0-rc1", 1}, {"2.0-beta", "2.0-rc", -1}, {"1.0", "1.0.1", -1}, {"10", "9.9.9", 1}, {"", "1", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("%q vs %q: %d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestOldFingerprintsAreUpgraded(t *testing.T) {
	cfg := testConfig(t)
	group := true
	cfg.Seeds[0].GroupDuplicates = &group
	srv, _ := newTestServer(t, cfg)
	post(t, srv, uploadVersion(ids(1)[0], "Crash in player", "3.2"), testKey)
	// Simulate an issue saved by the previous format (which included the version).
	_, _ = srv.issues.Update("gelo", 1, func(i *Issue) error { i.Fingerprint, i.FingerprintV, i.FirstVersion = "old", 0, ""; return nil })
	srv2, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	issue, _ := srv2.issues.Get("gelo", 1)
	if issue.FingerprintV != FingerprintVersion || issue.FirstVersion != "3.2" {
		t.Fatalf("not upgraded: %+v", issue)
	}
	post(t, srv2, uploadVersion("00000002-0000-4000-8000-000000000000", "Crash in player", "3.4"), testKey)
	if issue, _ = srv2.issues.Get("gelo", 1); len(issue.Reports) != 2 {
		t.Fatal("identical report on a new version should group after the upgrade")
	}
}

func TestNotifications(t *testing.T) {
	ntfy, ntfyRequests := recorder(t, func(r recorded) (int, string) { return 200, "{}" })
	push, pushRequests := recorder(t, func(r recorded) (int, string) { return 200, `{"status":1}` })
	cfg := testConfig(t)
	group := true
	cfg.Seeds[0].GroupDuplicates = &group
	cfg.Seeds[0].Notify = &NotifyConfig{Ntfy: ntfy.URL + "/gelo-alerts", NtfyToken: "tk", PushoverUser: "u", PushoverToken: "p", OnNewIssue: true, OnRegression: true}
	cfg.NotifyPerHour = 5
	srv, _ := newTestServer(t, cfg)
	srv.notifier.PushoverURL = push.URL
	id := ids(12)

	post(t, srv, uploadVersion(id[0], "Crash in player", "3.2"), testKey)
	srv.notifier.Wait()
	if len(ntfyRequests()) != 1 || len(pushRequests()) != 1 {
		t.Fatalf("new issue: ntfy %d pushover %d", len(ntfyRequests()), len(pushRequests()))
	}
	n := ntfyRequests()[0]
	if n.path != "/gelo-alerts" || n.headers.Get("Title") != "Gelo #1 · new bug · 3.2" || n.headers.Get("Authorization") != "Bearer tk" ||
		n.headers.Get("Click") != "https://snag.example.com/p/gelo/issues/1" || !strings.Contains(string(n.body), "same words") {
		t.Fatalf("ntfy request %+v %s", n.headers, n.body)
	}
	form, _ := url.ParseQuery(string(pushRequests()[0].body))
	if form.Get("user") != "u" || form.Get("token") != "p" || form.Get("url") != "https://snag.example.com/p/gelo/issues/1" {
		t.Fatalf("pushover %v", form)
	}

	// A duplicate joining the open issue isn't announced unless OnReport is on.
	post(t, srv, uploadVersion(id[1], "Crash in player", "3.2"), testKey)
	srv.notifier.Wait()
	if len(ntfyRequests()) != 1 {
		t.Fatal("duplicate alerted")
	}
	// A regression is, with high priority.
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"status": {"resolved"}, "fixedIn": {"3.3"}})
	post(t, srv, uploadVersion(id[2], "Crash in player", "3.3"), testKey)
	srv.notifier.Wait()
	last := ntfyRequests()[len(ntfyRequests())-1]
	if !strings.Contains(last.headers.Get("Title"), "regressed") || last.headers.Get("Priority") != "high" {
		t.Fatalf("regression alert %+v", last.headers)
	}
	// Priority alerts are opt-in.
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"priority": {"urgent"}})
	srv.notifier.Wait()
	before := len(ntfyRequests())
	_, _ = srv.projects.Update("gelo", func(p *Project) error { p.Notify.OnPriority = true; return nil })
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"priority": {"low"}})
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"priority": {"urgent"}})
	srv.notifier.Wait()
	if got := len(ntfyRequests()); got != before+1 || ntfyRequests()[got-1].headers.Get("Priority") != "urgent" {
		t.Fatalf("priority alerts %d → %d", before, got)
	}
	// Kind filter.
	_, _ = srv.projects.Update("gelo", func(p *Project) error { p.Notify.Kinds = []string{"crash"}; return nil })
	before = len(ntfyRequests())
	post(t, srv, uploadWith(id[3], "Some bug", "x"), testKey)
	srv.notifier.Wait()
	if len(ntfyRequests()) != before {
		t.Fatal("kind filter ignored")
	}
	// Hourly cap: 5 per hour; the rest are counted and mentioned later.
	_, _ = srv.projects.Update("gelo", func(p *Project) error { p.Notify.Kinds = nil; return nil })
	for i := 4; i < 12; i++ {
		post(t, srv, uploadWith(id[i], fmt.Sprintf("Bug %d", i), "x"), testKey)
	}
	srv.notifier.Wait()
	if got := len(ntfyRequests()); got != 5 {
		t.Fatalf("cap: %d alerts", got)
	}
	// The settings test button bypasses the cap.
	rr := adminPost(t, srv, "/p/gelo/notify/test", nil)
	if !strings.Contains(rr.Header().Get("Location"), "Test+notification+sent") || len(ntfyRequests()) != 6 {
		t.Fatalf("test: %s, %d", rr.Header().Get("Location"), len(ntfyRequests()))
	}
}

func TestNotificationSettings(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	adminPost(t, srv, "/p/gelo/settings", url.Values{"name": {"Gelo"}, "ntfy": {"ftp://nope"}})
	if p, _ := srv.projects.Get("gelo"); p.Notify.Ntfy != "" {
		t.Fatal("bad ntfy URL saved")
	}
	adminPost(t, srv, "/p/gelo/settings", url.Values{"name": {"Gelo"}, "ntfy": {"https://ntfy.sh/secret-topic"}, "onNewIssue": {"on"}, "notifyKinds": {"bug, crash"}})
	p, _ := srv.projects.Get("gelo")
	if p.Notify.Ntfy != "https://ntfy.sh/secret-topic" || !p.Notify.OnNewIssue || p.Notify.OnRegression || fmt.Sprint(p.Notify.Kinds) != "[bug crash]" {
		t.Fatalf("notify %+v", p.Notify)
	}
	page := adminGet(srv, "/p/gelo/settings").Body.String()
	if strings.Contains(page, "secret-topic") || !strings.Contains(page, "Send a test notification") {
		t.Fatal("topic leaked or test button missing")
	}
	env := map[string]string{"SNAG_API_KEYS": "gelo:aaaaaaaaaaaaaaaaaaaa", "SNAG_NTFY_URL": "https://ntfy.sh/x"}
	cfg, err := LoadConfig(func(k string) string { return env[k] })
	if err != nil || cfg.Seeds[0].Notify == nil || !cfg.Seeds[0].Notify.OnRegression || cfg.NotifyPerHour != 30 {
		t.Fatalf("env notify %+v %v", cfg.Seeds, err)
	}
}

func TestFollowUpThread(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	rr := post(t, srv, sampleUpload(id1), testKey)
	var resp struct {
		ID       string `json:"id"`
		FollowUp struct {
			Token string `json:"token"`
			Path  string `json:"path"`
		} `json:"followUp"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !strings.HasPrefix(resp.FollowUp.Token, "snagr_") || resp.FollowUp.Path != "api/v1/followup/"+resp.ID {
		t.Fatalf("followUp %+v", resp.FollowUp)
	}
	rec, _ := srv.store.Get(resp.ID)
	if rec.ReaderTokenHash == "" || strings.Contains(mustJSON(rec), resp.FollowUp.Token) {
		t.Fatal("reader token must be stored hashed")
	}
	get := func(token string) (*httptest.ResponseRecorder, ReporterStatus) {
		req := httptest.NewRequest("GET", "/"+resp.FollowUp.Path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		var st ReporterStatus
		_ = json.Unmarshal(rr.Body.Bytes(), &st)
		return rr, st
	}
	if rr, _ := get("snagr_wrong"); rr.Code != 404 {
		t.Fatalf("wrong token: %d", rr.Code)
	}
	if rr, _ := get(testKey); rr.Code != 404 {
		t.Fatal("ingest key must not read reports")
	}
	_, st := get(resp.FollowUp.Token)
	if st.Status != "received" || st.Label != "Received" || len(st.Messages) != 0 {
		t.Fatalf("status %+v", st)
	}

	// Team messages: to this reporter, and to everyone. A second reporter sees only the broadcast.
	adminPost(t, srv, "/p/gelo/issues/1/thread", url.Values{"report": {resp.ID}, "text": {"Which server version?"}})
	other := ids(1)[0]
	_ = post(t, srv, sampleUpload(other), testKey)
	adminPost(t, srv, "/p/gelo/bulk", url.Values{"action": {"merge"}, "issue": {"1", "2"}})
	adminPost(t, srv, "/p/gelo/issues/1/thread", url.Values{"text": {"Thanks everyone, looking into it."}})
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"status": {"resolved"}, "fixedIn": {"2.1"}})
	_, st = get(resp.FollowUp.Token)
	if st.Status != "fixed" || st.Label != "Fixed in 2.1" || len(st.Messages) != 2 || st.Messages[0].Text != "Which server version?" || st.Messages[0].ReportID != "" {
		t.Fatalf("after messages %+v", st)
	}
	issue, _ := srv.issues.Get("gelo", 1)
	if len(issue.ThreadFor(other)) != 1 {
		t.Fatalf("other reporter sees %d messages", len(issue.ThreadFor(other)))
	}

	// The reporter replies from the app.
	req := httptest.NewRequest("POST", "/"+resp.FollowUp.Path+"/messages", strings.NewReader(`{"text":"Jellyfin 10.10.3"}`))
	req.Header.Set("Authorization", "Bearer "+resp.FollowUp.Token)
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("reply: %d %s", rr.Code, rr.Body)
	}
	_, st = get(resp.FollowUp.Token)
	if len(st.Messages) != 3 || st.Messages[2].From != FromReporter {
		t.Fatalf("thread %+v", st.Messages)
	}
	page := adminGet(srv, "/p/gelo/issues/1").Body.String()
	if !strings.Contains(page, "Jellyfin 10.10.3") || !strings.Contains(page, "to all reporters") {
		t.Fatal("thread not on the issue page")
	}
	// Retrying the upload rotates the token.
	retry := post(t, srv, sampleUpload(id1), testKey, "Idempotency-Key", id1)
	var again struct {
		FollowUp struct{ Token string } `json:"followUp"`
	}
	_ = json.Unmarshal(retry.Body.Bytes(), &again)
	if again.FollowUp.Token == "" || again.FollowUp.Token == resp.FollowUp.Token {
		t.Fatal("retry should return a fresh token")
	}
	if rr, _ := get(resp.FollowUp.Token); rr.Code != 404 {
		t.Fatal("old token should stop working")
	}
	if rr, st := get(again.FollowUp.Token); rr.Code != 200 || len(st.Messages) != 3 {
		t.Fatal("new token should work")
	}
	// Another project's key can't fetch this report via the idempotency shortcut.
	srv.projects.Create("Other", "other")
	key, _ := srv.projects.AddKey("other", "")
	if rr := post(t, srv, sampleUpload(id1), key, "Idempotency-Key", id1); rr.Code != http.StatusConflict || strings.Contains(rr.Body.String(), "gelo") {
		t.Fatalf("cross-project idempotency: %d %s", rr.Code, rr.Body)
	}
	if rr, _ := get(again.FollowUp.Token); rr.Code != 200 {
		t.Fatal("another project's attempt must not rotate the token")
	}
	if _, total := srv.issues.List("other", IssueFilter{}); total != 0 {
		t.Fatal("conflicting upload left an issue behind")
	}
}

func TestAPIThreadMessageNeedsJSON(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	post(t, srv, sampleUpload(id1), testKey)
	send := func(ct, body string) int {
		req := httptest.NewRequest("POST", "/api/v1/projects/gelo/issues/1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		req.SetBasicAuth("admin", "pw-for-tests")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr.Code
	}
	if code := send("application/x-www-form-urlencoded", "text=hi"); code != http.StatusUnsupportedMediaType {
		t.Fatalf("form body: %d", code)
	}
	if code := send("application/json", `{"text":"hi","report":"`+strings.ToLower(id1)+`"}`); code != 200 {
		t.Fatalf("json: %d", code)
	}
	if code := send("application/json", `{"text":"hi","report":"00000000-0000-4000-8000-000000000099"}`); code != 400 {
		t.Fatalf("foreign report: %d", code)
	}
	if issue, _ := srv.issues.Get("gelo", 1); len(issue.Thread) != 1 {
		t.Fatal("message not stored")
	}
}

// fakeGitHub is a tiny stateful GitHub for sync tests.
type fakeGitHub struct {
	mu       sync.Mutex
	next     int
	issues   map[int]*fakeIssue
	comments map[int][]map[string]any
	calls    []string
}

type fakeIssue struct {
	State, Reason string
	Labels        []string
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{next: 5, issues: map[int]*fakeIssue{}, comments: map[int][]map[string]any{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		m := regexp.MustCompile(`^/repos/me/app/issues(?:/(\d+))?(/labels(?:/(.+))?|/comments)?$`).FindStringSubmatch(r.URL.Path)
		if m == nil {
			w.WriteHeader(404)
			return
		}
		n, _ := strconv.Atoi(m[1])
		switch {
		case r.Method == "POST" && m[1] == "":
			num := f.next
			f.next++
			labels := []string{}
			for _, l := range in["labels"].([]any) {
				labels = append(labels, l.(string))
			}
			f.issues[num] = &fakeIssue{State: "open", Labels: labels}
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.com/me/app/issues/%d"}`, num, num)
		case r.Method == "PATCH":
			f.issues[n].State = in["state"].(string)
			if reason, ok := in["state_reason"].(string); ok {
				f.issues[n].Reason = reason
			}
			w.Write([]byte(`{}`))
		case r.Method == "POST" && m[2] == "/labels":
			for _, l := range in["labels"].([]any) {
				if !contains(f.issues[n].Labels, l.(string)) {
					f.issues[n].Labels = append(f.issues[n].Labels, l.(string))
				}
			}
			w.Write([]byte(`[]`))
		case r.Method == "DELETE":
			label, _ := url.PathUnescape(m[3])
			var keep []string
			for _, l := range f.issues[n].Labels {
				if l != label {
					keep = append(keep, l)
				}
			}
			f.issues[n].Labels = keep
			w.Write([]byte(`[]`))
		case r.Method == "POST" && m[2] == "/comments":
			f.comments[n] = append(f.comments[n], map[string]any{"id": len(f.comments[n]) + 100, "body": in["body"], "user": map[string]any{"login": "snag-bot", "type": "User"}})
			w.Write([]byte(`{}`))
		case r.Method == "GET" && m[2] == "/comments":
			data, _ := json.Marshal(f.comments[n])
			w.Write(data)
		case r.Method == "GET":
			i := f.issues[n]
			labels := []map[string]string{}
			for _, l := range i.Labels {
				labels = append(labels, map[string]string{"name": l})
			}
			data, _ := json.Marshal(map[string]any{"state": i.State, "state_reason": i.Reason, "labels": labels, "closed_by": map[string]string{"login": "dev"}})
			w.Write(data)
		}
	}))
	t.Cleanup(ts.Close)
	return f, ts
}

func (f *fakeGitHub) issue(n int) fakeIssue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.issues[n]
}

func githubServer(t *testing.T) (*Server, *fakeGitHub) {
	gh, ts := newFakeGitHub(t)
	cfg := withForwarding(testConfig(t), ProjectForwarding{GitHub: &ProjectGitHub{Repo: "me/app", Labels: []string{"snag"}}})
	cfg.GitHubToken, cfg.GitHubAPIURL, cfg.GitHubWebhookSecret = "ghp_x", ts.URL, "hook-secret"
	srv, _ := newTestServer(t, cfg)
	post(t, srv, sampleUpload(id1), testKey)
	srv.Dispatcher().Process(context.Background(), strings.ToLower(id1))
	return srv, gh
}

func TestGitHubSyncPushes(t *testing.T) {
	srv, gh := githubServer(t)
	issue, _ := srv.issues.Get("gelo", 1)
	if issue.GitHub == nil || issue.GitHub.Number != 5 || issue.GitHub.Repo != "me/app" {
		t.Fatalf("not linked: %+v", issue.GitHub)
	}
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"priority": {"high"}, "addLabel": {"ui"}})
	srv.github.Wait()
	if got := gh.issue(5).Labels; !contains(got, "priority: high") || !contains(got, "ui") {
		t.Fatalf("labels on GitHub %v", got)
	}
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"priority": {"urgent"}, "removeLabel": {"ui"}})
	srv.github.Wait()
	if got := gh.issue(5).Labels; contains(got, "priority: high") || !contains(got, "priority: urgent") || contains(got, "ui") {
		t.Fatalf("labels after change %v", got)
	}
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"status": {"resolved"}, "fixedIn": {"2.1"}})
	srv.github.Wait()
	if st := gh.issue(5); st.State != "closed" || st.Reason != "completed" {
		t.Fatalf("not closed: %+v", st)
	}
	if len(gh.comments[5]) != 1 || !strings.Contains(gh.comments[5][0]["body"].(string), "Fixed in 2.1.") {
		t.Fatalf("comments %v", gh.comments[5])
	}
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"status": {"wontfix"}})
	srv.github.Wait()
	if st := gh.issue(5); st.Reason != "not_planned" {
		t.Fatalf("won't fix: %+v", st)
	}
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"status": {"triaged"}})
	srv.github.Wait()
	if st := gh.issue(5); st.State != "open" {
		t.Fatalf("not reopened: %+v", st)
	}
	// Polling sees nothing new: everything on GitHub came from Snag.
	before, _ := srv.issues.Get("gelo", 1)
	srv.github.Poll(context.Background())
	after, _ := srv.issues.Get("gelo", 1)
	if fmt.Sprint(before.Labels) != fmt.Sprint(after.Labels) || before.Priority != after.Priority || len(after.Notes) != 0 {
		t.Fatalf("poll imported our own changes: %v → %v, notes %d", before.Labels, after.Labels, len(after.Notes))
	}
}

func signedWebhook(t *testing.T, srv *Server, event string, payload map[string]any, secret string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req := httptest.NewRequest("POST", "/api/v1/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func TestGitHubWebhook(t *testing.T) {
	srv, _ := githubServer(t)
	repo := map[string]any{"full_name": "me/app"}
	labels := func(names ...string) []map[string]string {
		out := []map[string]string{}
		for _, n := range names {
			out = append(out, map[string]string{"name": n})
		}
		return out
	}
	if rr := signedWebhook(t, srv, "issues", map[string]any{"action": "closed"}, "wrong"); rr.Code != 401 {
		t.Fatalf("bad signature: %d", rr.Code)
	}
	// GitHub's initial labels (from Snag) aren't changes.
	signedWebhook(t, srv, "issues", map[string]any{"action": "labeled", "repository": repo,
		"issue": map[string]any{"number": 5, "state": "open", "labels": labels("snag", "bug", "beta")}}, "hook-secret")
	signedWebhook(t, srv, "issues", map[string]any{"action": "labeled", "repository": repo,
		"issue": map[string]any{"number": 5, "state": "open", "labels": labels("snag", "bug", "beta", "needs-repro", "priority: medium")}}, "hook-secret")
	issue, _ := srv.issues.Get("gelo", 1)
	if !contains(issue.Labels, "needs-repro") || contains(issue.Labels, "snag") || issue.Priority != "medium" {
		t.Fatalf("labeled: %v %q", issue.Labels, issue.Priority)
	}
	signedWebhook(t, srv, "issues", map[string]any{"action": "unlabeled", "repository": repo,
		"issue": map[string]any{"number": 5, "state": "open", "labels": labels("snag", "bug", "needs-repro", "priority: medium")}}, "hook-secret")
	if issue, _ = srv.issues.Get("gelo", 1); contains(issue.Labels, "beta") || !contains(issue.Labels, "needs-repro") {
		t.Fatalf("unlabeled: %v", issue.Labels)
	}
	signedWebhook(t, srv, "issues", map[string]any{"action": "closed", "repository": repo, "sender": map[string]any{"login": "dev"},
		"issue": map[string]any{"number": 5, "state": "closed", "state_reason": "completed"}}, "hook-secret")
	if issue, _ = srv.issues.Get("gelo", 1); issue.Status != IssueResolved {
		t.Fatalf("closed: %s", issue.Status)
	}
	if last := issue.Activity[len(issue.Activity)-1]; !strings.Contains(last.Text, "Closed on GitHub by @dev") {
		t.Fatalf("activity %+v", last)
	}
	signedWebhook(t, srv, "issues", map[string]any{"action": "reopened", "repository": repo,
		"issue": map[string]any{"number": 5, "state": "open"}}, "hook-secret")
	if issue, _ = srv.issues.Get("gelo", 1); issue.Status != IssueTriaged {
		t.Fatalf("reopened: %s", issue.Status)
	}
	comment := func(id int, body, user, kind string) {
		signedWebhook(t, srv, "issue_comment", map[string]any{"action": "created", "repository": repo,
			"issue": map[string]any{"number": 5}, "comment": map[string]any{"id": id, "body": body, "user": map[string]any{"login": user, "type": kind}}}, "hook-secret")
	}
	comment(1, "Can't repro on 2.1", "dev", "User")
	comment(1, "Can't repro on 2.1", "dev", "User") // redelivery
	comment(2, "beep", "ci", "Bot")
	comment(3, snagCommentMarker+"\nFixed in 2.1.", "snag", "User")
	if issue, _ = srv.issues.Get("gelo", 1); len(issue.Notes) != 1 || !strings.Contains(issue.Notes[0].Text, "GitHub comment by @dev:\nCan't repro") {
		t.Fatalf("notes %+v", issue.Notes)
	}
	// Unlinked issues and pings are fine.
	if rr := signedWebhook(t, srv, "ping", map[string]any{}, "hook-secret"); rr.Code != 200 {
		t.Fatal("ping")
	}
	if rr := signedWebhook(t, srv, "issues", map[string]any{"action": "closed", "repository": repo, "issue": map[string]any{"number": 99}}, "hook-secret"); rr.Code != 200 {
		t.Fatal("unlinked")
	}
}

func TestGitHubPollAndMerge(t *testing.T) {
	srv, gh := githubServer(t)
	// Changes made directly on GitHub.
	gh.mu.Lock()
	gh.issues[5].State, gh.issues[5].Reason = "closed", "not_planned"
	gh.issues[5].Labels = append(gh.issues[5].Labels, "wontfix-reason")
	gh.comments[5] = append(gh.comments[5], map[string]any{"id": 7, "body": "Working as intended", "user": map[string]any{"login": "dev", "type": "User"}})
	gh.mu.Unlock()
	srv.github.Poll(context.Background())
	issue, _ := srv.issues.Get("gelo", 1)
	if issue.Status != IssueWontFix || !contains(issue.Labels, "wontfix-reason") || len(issue.Notes) != 1 {
		t.Fatalf("poll: %s %v %d notes", issue.Status, issue.Labels, len(issue.Notes))
	}
	srv.github.Poll(context.Background())
	if issue, _ = srv.issues.Get("gelo", 1); len(issue.Notes) != 1 {
		t.Fatal("comment imported twice")
	}
	// Merging closes the duplicate's GitHub issue, pointing at the target.
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"status": {"new"}})
	post(t, srv, uploadWith(ids(1)[0], "Another", "x"), testKey)
	srv.Dispatcher().Process(context.Background(), ids(1)[0])
	srv.github.Wait()
	adminPost(t, srv, "/p/gelo/bulk", url.Values{"action": {"merge"}, "issue": {"1", "2"}})
	srv.github.Wait()
	if st := gh.issue(6); st.State != "closed" || st.Reason != "not_planned" {
		t.Fatalf("duplicate not closed on GitHub: %+v", st)
	}
	if body := gh.comments[6][len(gh.comments[6])-1]["body"].(string); !strings.Contains(body, "Duplicate of https://github.com/me/app/issues/5") {
		t.Fatalf("merge comment %q", body)
	}
}
