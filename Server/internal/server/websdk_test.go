package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestWebSDKAndOrigins(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, httptest.NewRequest("GET", "/sdk/snag.js", nil))
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/javascript") || !strings.Contains(rr.Body.String(), "Snag web SDK") {
		t.Fatalf("sdk: %d %s", rr.Code, rr.Header())
	}

	preflight := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("OPTIONS", "/api/v1/reports", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	if rr := preflight("https://site.example.com"); rr.Code != http.StatusForbidden {
		t.Fatalf("preflight before any origin is allowed: %d", rr.Code)
	}
	// A browser upload from an unlisted origin is refused even with a valid key.
	if rr := post(t, srv, sampleUpload(id1), testKey, "Origin", "https://site.example.com"); rr.Code != http.StatusForbidden {
		t.Fatalf("unlisted origin: %d", rr.Code)
	}

	if rr := adminPost(t, srv, "/p/gelo/settings", url.Values{"name": {"Gelo"}, "webOrigins": {"https://Site.example.com/path\nhttp://localhost:3000"}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rr.Code, rr.Body)
	}
	p, _ := srv.Projects().Get("gelo")
	if strings.Join(p.WebOrigins, ",") != "https://site.example.com,http://localhost:3000" {
		t.Fatalf("origins %v", p.WebOrigins)
	}
	rr = preflight("https://site.example.com")
	if rr.Code != http.StatusNoContent || rr.Header().Get("Access-Control-Allow-Origin") != "https://site.example.com" ||
		!strings.Contains(rr.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %d %v", rr.Code, rr.Header())
	}
	rr = post(t, srv, sampleUpload(id1), testKey, "Origin", "https://site.example.com")
	if rr.Code != http.StatusCreated || rr.Header().Get("Access-Control-Allow-Origin") != "https://site.example.com" {
		t.Fatalf("allowed origin: %d %v", rr.Code, rr.Header())
	}
	// Native apps send no Origin and are unaffected.
	if rr := post(t, srv, sampleUpload(id2), testKey); rr.Code != http.StatusCreated {
		t.Fatalf("native: %d", rr.Code)
	}
	if rr := adminPost(t, srv, "/p/gelo/settings", url.Values{"name": {"Gelo"}, "webOrigins": {"javascript:alert(1)"}}); rr.Code == http.StatusSeeOther {
		t.Fatal("bad origin saved")
	}
}
