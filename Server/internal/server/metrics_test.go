package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMetricsAndStorageLimits(t *testing.T) {
	cfg := testConfig(t)
	cfg.MetricsToken = "metrics-token-for-tests"
	srv, store := newTestServer(t, cfg)
	post(t, srv, sampleUpload(id1), testKey)
	post(t, srv, sampleUpload(id2), "wrong-key-xxxxxxxxxxxx")

	scrape := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/metrics", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	if rr := scrape("Bearer nope"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", rr.Code)
	}
	body := scrape("Bearer metrics-token-for-tests").Body.String()
	for _, want := range []string{
		`snag_reports_received_total{project="gelo",kind="bug"} 1`,
		`snag_http_requests_total{route="ingest",code="401"} 1`,
		`snag_issues{project="gelo",status="new"} 1`,
		`snag_reports{project="gelo"} 1`,
		`snag_storage_bytes{project="gelo"} 85`,
		"# TYPE snag_issues gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
	if rr := adminGet(srv, "/metrics"); rr.Code != 200 {
		t.Fatalf("admin scrape: %d", rr.Code)
	}

	// A 1 MB limit: the next report fits; once over, media is dropped and text kept.
	adminPost(t, srv, "/p/gelo/settings", url.Values{"name": {"Gelo"}, "storageLimitMB": {"1"}})
	big := sampleUpload(ids(2)[0])
	big.files["attachment0"] = append(append([]byte{}, pngBytes...), make([]byte, 1<<20)...)
	if rr := post(t, srv, big, testKey); rr.Code != http.StatusCreated {
		t.Fatalf("big upload: %d", rr.Code)
	}
	rec, _ := store.Get(ids(2)[0])
	if rec.DroppedForQuota != 1 || len(rec.Files) != 1 || rec.Files[0].Role != "log" {
		t.Fatalf("quota: dropped %d, files %+v", rec.DroppedForQuota, rec.Files)
	}
	if !strings.Contains(adminGet(srv, "/p/gelo/issues/"+itoa(rec.Issue)).Body.String(), "1 attachment not kept") {
		t.Error("issue page doesn't say the attachment was dropped")
	}
	if !strings.Contains(adminGet(srv, "/p/gelo/settings").Body.String(), "of 1.0 MB") {
		t.Error("settings don't show usage against the limit")
	}
}
