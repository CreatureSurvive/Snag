package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "test-ingest-key-0123456789"

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Addr: ":0", DataDir: t.TempDir(), PublicURL: "https://snag.example.com",
		APIKeys:   map[string]string{testKey: "gelo"},
		AdminUser: "admin", AdminPassword: "pw-for-tests",
		MaxUploadBytes: 2 << 20, RatePerHour: 100,
	}
}

func newTestServer(t *testing.T, cfg Config) (*Server, *Store) {
	t.Helper()
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, store, nil), store
}

type upload struct {
	manifest map[string]any
	files    map[string][]byte // part name -> bytes
	types    map[string]string
}

func sampleUpload(id string) upload {
	return upload{
		manifest: map[string]any{
			"id": id, "createdAt": "2026-10-27T10:00:00.123Z", "kind": "bug",
			"title": "Playback stalls", "message": "It stops after 3 seconds.", "contact": "me@example.com",
			"tags": []string{"beta"},
			"context": map[string]any{
				"app":    map[string]any{"name": "Gelo", "version": "2.0", "build": "41", "bundleIdentifier": "com.example.gelo"},
				"device": map[string]any{"model": "iPhone17,1", "system": "iOS", "systemVersion": "26.1", "batteryLevel": 0.42, "lowPowerMode": false},
				"custom": map[string]string{"server": "jellyfin.local"},
			},
			"attachments": []map[string]any{
				{"id": "A0000000-0000-0000-0000-000000000000", "role": "screenshot", "filename": "screenshot.png", "contentType": "image/png", "size": len(pngBytes), "part": "attachment0"},
				{"id": "A1000000-0000-0000-0000-000000000000", "role": "log", "filename": "../../log.txt", "contentType": "text/plain; charset=utf-8", "size": 13, "part": "attachment1"},
			},
		},
		files: map[string][]byte{"attachment0": pngBytes, "attachment1": []byte("line 1\nline 2")},
		types: map[string]string{"attachment0": "image/png", "attachment1": "text/plain; charset=utf-8"},
	}
}

func (u upload) body() (*bytes.Buffer, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	data, _ := json.Marshal(u.manifest)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="report"; filename="report.json"`)
	h.Set("Content-Type", "application/json")
	pw, _ := mw.CreatePart(h)
	_, _ = pw.Write(data)
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("attachment%d", i)
		content, ok := u.files[name]
		if !ok {
			continue
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename="f%d"`, name, i))
		h.Set("Content-Type", u.types[name])
		pw, _ := mw.CreatePart(h)
		_, _ = pw.Write(content)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func post(t *testing.T, h http.Handler, u upload, key string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := u.body()
	req := httptest.NewRequest("POST", "/api/v1/reports", body)
	req.Header.Set("Content-Type", ct)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func adminGet(h http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.SetBasicAuth("admin", "pw-for-tests")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

const id1 = "2C1A4F51-FD33-442C-AB20-C90630FC6A28"

func TestIngestStoresReport(t *testing.T) {
	srv, store := newTestServer(t, testConfig(t))
	rr := post(t, srv, sampleUpload(id1), testKey)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var resp map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	wantID := strings.ToLower(id1)
	if resp["id"] != wantID || resp["url"] != "https://snag.example.com/reports/"+wantID {
		t.Fatalf("response %v", resp)
	}
	rec, err := store.Get(id1)
	if err != nil {
		t.Fatal(err)
	}
	if rec.App != "gelo" || rec.Status != StatusOpen || rec.Title != "Playback stalls" {
		t.Fatalf("record %+v", rec)
	}
	if want := time.Date(2026, 10, 27, 10, 0, 0, 123e6, time.UTC); !rec.CreatedAt.Equal(want) {
		t.Fatalf("createdAt %v", rec.CreatedAt)
	}
	if len(rec.Files) != 2 {
		t.Fatalf("files %+v", rec.Files)
	}
	if rec.Files[0].ContentType != "image/png" || rec.Files[0].Role != "screenshot" || rec.Files[0].Size != int64(len(pngBytes)) {
		t.Fatalf("screenshot %+v", rec.Files[0])
	}
	if rec.Files[1].Filename != "log.txt" {
		t.Fatalf("filename not sanitized: %q", rec.Files[1].Filename)
	}
	path, _, _ := store.FilePath(id1, rec.Files[1].Name)
	if data, _ := os.ReadFile(path); string(data) != "line 1\nline 2" {
		t.Fatalf("log contents %q", data)
	}
	if rec.Context.Device["model"] != "iPhone17,1" {
		t.Fatalf("device %v", rec.Context.Device)
	}
}

func TestIngestIsIdempotent(t *testing.T) {
	srv, store := newTestServer(t, testConfig(t))
	if rr := post(t, srv, sampleUpload(id1), testKey); rr.Code != 201 {
		t.Fatal(rr.Body)
	}
	// Same ID again, without the header: the body's ID still dedups.
	if rr := post(t, srv, sampleUpload(id1), testKey); rr.Code != 200 {
		t.Fatalf("duplicate: %d %s", rr.Code, rr.Body)
	}
	// With the header, the body isn't even read.
	req := httptest.NewRequest("POST", "/api/v1/reports", strings.NewReader("garbage"))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Idempotency-Key", id1)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("idempotency key: %d %s", rr.Code, rr.Body)
	}
	if _, total := store.List(Filter{}); total != 1 {
		t.Fatalf("stored %d reports", total)
	}
	entries, _ := os.ReadDir(filepath.Join(store.dir, "tmp"))
	if len(entries) != 0 {
		t.Fatalf("staging not cleaned: %d", len(entries))
	}
}

func TestIngestRejectsBadRequests(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	cases := []struct {
		name   string
		mutate func(*upload)
		key    string
		status int
	}{
		{"no key", func(*upload) {}, "", 401},
		{"wrong key", func(*upload) {}, "nope-nope-nope-nope-nope", 401},
		{"bad id", func(u *upload) { u.manifest["id"] = "../../etc" }, testKey, 400},
		{"empty", func(u *upload) { u.manifest["title"] = ""; u.manifest["message"] = " " }, testKey, 400},
		{"too big", func(u *upload) { u.files["attachment0"] = bytes.Repeat([]byte("x"), 3<<20) }, testKey, 413},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := sampleUpload("3C1A4F51-FD33-442C-AB20-C90630FC6A28")
			c.mutate(&u)
			if rr := post(t, srv, u, c.key); rr.Code != c.status {
				t.Fatalf("want %d, got %d: %s", c.status, rr.Code, rr.Body)
			}
		})
	}
	req := httptest.NewRequest("POST", "/api/v1/reports", strings.NewReader(`{"id":"x"}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("json body: %d", rr.Code)
	}
}

func TestContentTypesAreSniffed(t *testing.T) {
	srv, store := newTestServer(t, testConfig(t))
	u := sampleUpload(id1)
	// Claims to be a PNG but is HTML; claims SVG (scriptable).
	u.files["attachment0"] = []byte("<html><script>alert(1)</script></html>")
	u.manifest["attachments"] = append(u.manifest["attachments"].([]map[string]any),
		map[string]any{"id": "A2000000-0000-0000-0000-000000000000", "role": "file", "filename": "x.svg", "contentType": "image/svg+xml", "size": 10, "part": "attachment2"})
	u.files["attachment2"] = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	if rr := post(t, srv, u, testKey); rr.Code != 201 {
		t.Fatal(rr.Body)
	}
	rec, _ := store.Get(id1)
	for _, f := range rec.Files {
		if f.Role == "screenshot" || f.Filename == "x.svg" {
			if f.IsImage() {
				t.Fatalf("%s kept image type %s", f.Filename, f.ContentType)
			}
		}
	}
	rr := adminGet(srv, "/reports/"+rec.ID+"/files/"+rec.Files[0].Name)
	if got := rr.Header().Get("Content-Type"); strings.HasPrefix(got, "text/html") || strings.HasPrefix(got, "image/") {
		t.Fatalf("served as %s", got)
	}
	if !strings.Contains(rr.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("attachments need a sandbox CSP")
	}
}

func TestRateLimit(t *testing.T) {
	cfg := testConfig(t)
	cfg.RatePerHour = 2
	srv, _ := newTestServer(t, cfg)
	ids := []string{"10000000-0000-0000-0000-000000000001", "10000000-0000-0000-0000-000000000002", "10000000-0000-0000-0000-000000000003"}
	codes := []int{}
	for _, id := range ids {
		codes = append(codes, post(t, srv, sampleUpload(id), testKey).Code)
	}
	if fmt.Sprint(codes) != "[201 201 429]" {
		t.Fatalf("codes %v", codes)
	}
	// Retries of stored reports aren't limited.
	if rr := post(t, srv, sampleUpload(ids[0]), testKey, "Idempotency-Key", ids[0]); rr.Code != 200 {
		t.Fatalf("retry limited: %d", rr.Code)
	}
}

func TestStoreReloadsFromDisk(t *testing.T) {
	cfg := testConfig(t)
	srv, _ := newTestServer(t, cfg)
	post(t, srv, sampleUpload(id1), testKey)
	// Leftover from a crash.
	_ = os.MkdirAll(filepath.Join(cfg.DataDir, "reports", strings.ToLower("4C1A4F51-FD33-442C-AB20-C90630FC6A28")), 0o750)
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, total := store.List(Filter{}); total != 1 {
		t.Fatalf("reloaded %d", total)
	}
	if _, err := store.Get(id1); err != nil {
		t.Fatal(err)
	}
}

func TestDashboardRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	for _, path := range []string{"/", "/reports/" + id1, "/api/v1/reports"} {
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

func TestDashboardPagesRender(t *testing.T) {
	srv, store := newTestServer(t, testConfig(t))
	u := sampleUpload(id1)
	u.manifest["message"] = "<script>alert('x')</script>"
	post(t, srv, u, testKey)
	list := adminGet(srv, "/")
	if list.Code != 200 || !strings.Contains(list.Body.String(), "Playback stalls") {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}
	if !strings.Contains(list.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("dashboard CSP missing")
	}
	page := adminGet(srv, "/reports/"+strings.ToLower(id1))
	body := page.Body.String()
	if page.Code != 200 {
		t.Fatalf("report: %d %s", page.Code, body)
	}
	if strings.Contains(body, "<script>alert") {
		t.Fatal("message not escaped")
	}
	for _, want := range []string{"line 2", "iPhone17,1", "iOS 26.1", "42%", "jellyfin.local", "me@example.com", "<img src=\"/reports/"} {
		if !strings.Contains(body, want) {
			t.Errorf("report page missing %q", want)
		}
	}
	search := adminGet(srv, "/?q=jellyfin")
	if !strings.Contains(search.Body.String(), "Playback stalls") {
		t.Fatal("search by custom context failed")
	}
	none := adminGet(srv, "/?q=zzz-nothing")
	if strings.Contains(none.Body.String(), "Playback stalls") {
		t.Fatal("search matched everything")
	}
	rec, _ := store.Get(id1)
	img := adminGet(srv, "/reports/"+rec.ID+"/files/"+rec.Files[0].Name)
	if img.Code != 200 || img.Header().Get("Content-Type") != "image/png" || !bytes.Equal(img.Body.Bytes(), pngBytes) {
		t.Fatalf("image: %d %s", img.Code, img.Header().Get("Content-Type"))
	}
	if adminGet(srv, "/reports/"+rec.ID+"/files/../report.json").Code == 200 {
		t.Fatal("path traversal")
	}
	api := adminGet(srv, "/api/v1/reports?q=stalls")
	if !strings.Contains(api.Body.String(), `"total":1`) {
		t.Fatalf("api: %s", api.Body)
	}
}

func TestStatusChangeNeedsCSRF(t *testing.T) {
	srv, store := newTestServer(t, testConfig(t))
	post(t, srv, sampleUpload(id1), testKey)
	id := strings.ToLower(id1)
	form := func(values url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/reports/"+id+"/status", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth("admin", "pw-for-tests")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	if rr := form(url.Values{"status": {"closed"}}); rr.Code != 403 {
		t.Fatalf("no token: %d", rr.Code)
	}
	page := adminGet(srv, "/reports/"+id).Body.String()
	token := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`).FindStringSubmatch(page)
	if token == nil {
		t.Fatal("no csrf token in page")
	}
	rr := form(url.Values{"status": {"closed"}, "csrf": {token[1]}, "return": {"https://evil.example"}})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/reports/"+id {
		t.Fatalf("close: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	if rec, _ := store.Get(id); rec.Status != StatusClosed {
		t.Fatalf("status %s", rec.Status)
	}
	if list := adminGet(srv, "/").Body.String(); strings.Contains(list, "Playback stalls") {
		t.Fatal("closed report in open list")
	}
}

// MARK: Forwarding

type recorded struct {
	method, path, contentType string
	headers                   http.Header
	body                      []byte
}

func recorder(t *testing.T, respond func(r recorded) (int, string)) (*httptest.Server, func() []recorded) {
	var mu sync.Mutex
	var all []recorded
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec := recorded{r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Clone(), body}
		mu.Lock()
		all = append(all, rec)
		mu.Unlock()
		status, resp := respond(rec)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(ts.Close)
	return ts, func() []recorded { mu.Lock(); defer mu.Unlock(); return append([]recorded{}, all...) }
}

func ingestWith(t *testing.T, cfg Config) (*Server, *Store, string) {
	srv, store := newTestServer(t, cfg)
	if rr := post(t, srv, sampleUpload(id1), testKey); rr.Code != 201 {
		t.Fatal(rr.Body)
	}
	return srv, store, strings.ToLower(id1)
}

func TestGitHubForwarding(t *testing.T) {
	gh, requests := recorder(t, func(r recorded) (int, string) {
		switch {
		case r.method == "GET" && strings.HasSuffix(r.path, "/git/ref/heads/snag-files"):
			return 404, `{"message":"Not Found"}`
		case r.method == "GET" && r.path == "/repos/me/app":
			return 200, `{"default_branch":"main"}`
		case r.method == "GET" && strings.HasSuffix(r.path, "/git/ref/heads/main"):
			return 200, `{"object":{"sha":"abc"}}`
		case r.method == "POST" && strings.HasSuffix(r.path, "/git/refs"):
			return 201, `{}`
		case r.method == "PUT":
			return 201, `{}`
		case r.method == "POST" && r.path == "/repos/me/app/issues":
			return 201, `{"number":5,"html_url":"https://github.com/me/app/issues/5"}`
		}
		return 500, "unexpected"
	})
	cfg := testConfig(t)
	cfg.GitHub = &GitHubConfig{Token: "ghp_x", Owner: "me", Repo: "app", Labels: []string{"snag"}, AttachmentBranch: "snag-files", APIURL: gh.URL}
	srv, store, id := ingestWith(t, cfg)
	srv.Dispatcher().Process(context.Background(), id)

	rec, _ := store.Get(id)
	st := rec.Forwarding["github"]
	if st == nil || !st.Delivered || st.URL != "https://github.com/me/app/issues/5" {
		t.Fatalf("state %+v", st)
	}
	reqs := requests()
	var issue map[string]any
	_ = json.Unmarshal(reqs[len(reqs)-1].body, &issue)
	body := issue["body"].(string)
	for _, want := range []string{"It stops after 3 seconds.", "[Open in Snag](https://snag.example.com/reports/" + id + ")", "?raw=true)", "line 2", "| Device | iPhone17,1 |", "<!-- snag:" + id + " -->"} {
		if !strings.Contains(body, want) {
			t.Errorf("issue body missing %q:\n%s", want, body)
		}
	}
	if fmt.Sprint(issue["labels"]) != "[snag bug]" {
		t.Fatalf("labels %v", issue["labels"])
	}
	if reqs[0].headers.Get("Authorization") != "Bearer ghp_x" {
		t.Fatal("auth header")
	}
	// Only images are committed.
	puts := 0
	for _, r := range reqs {
		if r.method == "PUT" {
			puts++
		}
	}
	if puts != 1 {
		t.Fatalf("puts %d", puts)
	}
}

func TestForwardRetriesThenGivesUpOnPermanentErrors(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	hook, _ := recorder(t, func(r recorded) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return 503, "down"
		}
		return 200, "ok"
	})
	bad, _ := recorder(t, func(r recorded) (int, string) { return 404, "no such webhook" })
	cfg := testConfig(t)
	cfg.Webhook = &WebhookConfig{URL: hook.URL, Secret: "s"}
	cfg.Slack = bad.URL
	srv, store, id := ingestWith(t, cfg)
	d := srv.Dispatcher()
	d.Backoff = func(int) time.Duration { return 0 }

	d.Process(context.Background(), id)
	rec, _ := store.Get(id)
	if st := rec.Forwarding["webhook"]; st.Delivered || st.Attempts != 1 || st.GaveUp {
		t.Fatalf("webhook after 1: %+v", st)
	}
	if st := rec.Forwarding["slack"]; !st.GaveUp || !strings.Contains(st.Error, "404") {
		t.Fatalf("slack: %+v", st)
	}
	if len(store.Pending()) != 1 {
		t.Fatal("should still be pending")
	}
	d.Process(context.Background(), id)
	rec, _ = store.Get(id)
	if st := rec.Forwarding["webhook"]; !st.Delivered || st.Attempts != 2 {
		t.Fatalf("webhook after 2: %+v", st)
	}
	if len(store.Pending()) != 0 {
		t.Fatal("nothing should be pending")
	}
	// Manual retry resets the given-up forward.
	if err := d.Retry(id); err != nil {
		t.Fatal(err)
	}
	rec, _ = store.Get(id)
	if st := rec.Forwarding["slack"]; st.GaveUp || st.Attempts != 0 {
		t.Fatalf("after retry: %+v", st)
	}
}

func TestWebhookSignatureAndPayload(t *testing.T) {
	hook, requests := recorder(t, func(r recorded) (int, string) { return 204, "" })
	cfg := testConfig(t)
	cfg.Webhook = &WebhookConfig{URL: hook.URL, Secret: "s3cret"}
	srv, _, id := ingestWith(t, cfg)
	srv.Dispatcher().Process(context.Background(), id)
	r := requests()[0]
	ts := r.headers.Get("X-Snag-Timestamp")
	if r.headers.Get("X-Snag-Signature") != "sha256="+Sign(r.body, ts, "s3cret") {
		t.Fatal("bad signature")
	}
	var payload struct {
		Report Record `json:"report"`
		Files  []struct {
			Filename string `json:"filename"`
			Data     string `json:"data"`
		} `json:"files"`
	}
	if err := json.Unmarshal(r.body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Report.ID != id || len(payload.Files) != 2 {
		t.Fatalf("payload %+v", payload.Report)
	}
	if data, _ := base64.StdEncoding.DecodeString(payload.Files[1].Data); string(data) != "line 1\nline 2" {
		t.Fatal("file data")
	}
}

func TestDiscordForwarding(t *testing.T) {
	discord, requests := recorder(t, func(r recorded) (int, string) { return 200, `{"id":"1"}` })
	cfg := testConfig(t)
	cfg.Discord = discord.URL + "/api/webhooks/1/abc"
	srv, _, id := ingestWith(t, cfg)
	srv.Dispatcher().Process(context.Background(), id)
	r := requests()[0]
	_, params, _ := mime.ParseMediaType(r.contentType)
	mr := multipart.NewReader(bytes.NewReader(r.body), params["boundary"])
	var names []string
	var payload map[string]any
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		names = append(names, p.FormName())
		if p.FormName() == "payload_json" {
			_ = json.NewDecoder(p).Decode(&payload)
		}
	}
	if fmt.Sprint(names) != "[payload_json files[0] files[1]]" {
		t.Fatalf("parts %v", names)
	}
	embed := payload["embeds"].([]any)[0].(map[string]any)
	if embed["image"].(map[string]any)["url"] != "attachment://screenshot.png" || embed["url"] != "https://snag.example.com/reports/"+id {
		t.Fatalf("embed %v", embed)
	}
}

func TestEmailMessage(t *testing.T) {
	cfg := testConfig(t)
	cfg.SMTP = &SMTPConfig{Host: "smtp.example.com", Port: 587, From: "Snag <snag@example.com>", To: []string{"dev@example.com"}, TLS: "starttls"}
	var sent []byte
	srv, store := newTestServer(t, cfg)
	for _, f := range srv.forwarder.forwarders {
		if e, ok := f.(*EmailForwarder); ok {
			e.send = func(_ SMTPConfig, msg []byte) error { sent = msg; return nil }
		}
	}
	post(t, srv, sampleUpload(id1), testKey)
	id := strings.ToLower(id1)
	srv.Dispatcher().Process(context.Background(), id)
	if rec, _ := store.Get(id); !rec.Forwarding["email"].Delivered {
		t.Fatalf("not delivered: %+v", rec.Forwarding["email"])
	}
	msg := string(sent)
	for _, want := range []string{"To: dev@example.com", "Reply-To: me@example.com", "Subject: [Gelo 2.0 (41)] Playback stalls", `filename=screenshot.png`, "multipart/mixed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("email missing %q", want)
		}
	}
	// Header injection through the contact field is refused.
	rec, _ := store.Get(id)
	rec.Contact = "x@example.com\r\nBcc: victim@example.com"
	out, _ := BuildEmail(*cfg.SMTP, rec, func(StoredFile) ([]byte, error) { return nil, nil }, "")
	head, _, _ := strings.Cut(string(out), "\r\n\r\n")
	if strings.Contains(head, "Bcc:") || strings.Contains(head, "Reply-To") {
		t.Fatalf("header injection:\n%s", head)
	}
}

func TestConfigParsing(t *testing.T) {
	env := map[string]string{
		"SNAG_API_KEYS":        "gelo:aaaaaaaaaaaaaaaaaaaa, bbbbbbbbbbbbbbbbbbbb",
		"SNAG_ADMIN_PASSWORD":  "pw",
		"SNAG_GITHUB_TOKEN":    "t",
		"SNAG_GITHUB_REPO":     "me/app",
		"SNAG_GITHUB_LABELS":   "snag, triage",
		"SNAG_SMTP_HOST":       "mail.example.com",
		"SNAG_SMTP_PORT":       "465",
		"SNAG_SMTP_FROM":       "a@example.com",
		"SNAG_SMTP_TO":         "b@example.com,c@example.com",
		"SNAG_RETENTION_DAYS":  "30",
		"SNAG_PUBLIC_URL":      "https://snag.example.com/",
		"SNAG_DISCORD_WEBHOOK": "https://discord.com/api/webhooks/1/x",
	}
	cfg, err := LoadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKeys["aaaaaaaaaaaaaaaaaaaa"] != "gelo" || cfg.APIKeys["bbbbbbbbbbbbbbbbbbbb"] != "default" {
		t.Fatalf("keys %v", cfg.APIKeys)
	}
	if cfg.GitHub.Owner != "me" || fmt.Sprint(cfg.GitHub.Labels) != "[snag triage]" {
		t.Fatalf("github %+v", cfg.GitHub)
	}
	if cfg.SMTP.TLS != "tls" || len(cfg.SMTP.To) != 2 {
		t.Fatalf("smtp %+v", cfg.SMTP)
	}
	if cfg.PublicURL != "https://snag.example.com" || cfg.Retention() != 30*24*time.Hour {
		t.Fatal("public url / retention")
	}
	for name, bad := range map[string]map[string]string{
		"no keys":    {},
		"short key":  {"SNAG_API_KEYS": "short"},
		"bad repo":   {"SNAG_API_KEYS": "aaaaaaaaaaaaaaaaaaaa", "SNAG_GITHUB_TOKEN": "t", "SNAG_GITHUB_REPO": "nope"},
		"smtp no to": {"SNAG_API_KEYS": "aaaaaaaaaaaaaaaaaaaa", "SNAG_SMTP_HOST": "h", "SNAG_SMTP_FROM": "a@b.c"},
	} {
		if _, err := LoadConfig(func(k string) string { return bad[k] }); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	// _FILE secrets.
	path := filepath.Join(t.TempDir(), "keys")
	_ = os.WriteFile(path, []byte("cccccccccccccccccccc\n"), 0o600)
	cfg, err = LoadConfig(func(k string) string { return map[string]string{"SNAG_API_KEYS_FILE": path}[k] })
	if err != nil || cfg.APIKeys["cccccccccccccccccccc"] != "default" {
		t.Fatalf("file secret: %v %v", err, cfg.APIKeys)
	}
}

func TestPrune(t *testing.T) {
	srv, store := newTestServer(t, testConfig(t))
	post(t, srv, sampleUpload(id1), testKey)
	if n := store.Prune(time.Now().Add(-time.Hour)); n != 0 {
		t.Fatal("pruned fresh report")
	}
	if n := store.Prune(time.Now().Add(time.Hour)); n != 1 {
		t.Fatal("didn't prune")
	}
	if _, total := store.List(Filter{}); total != 0 {
		t.Fatal("still listed")
	}
}

func TestSanitizeFilename(t *testing.T) {
	for in, want := range map[string]string{"../../etc/passwd": "passwd", `C:\x\log.txt`: "log.txt", "a\"b\r\n.png": "ab.png", "...": "attachment"} {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
