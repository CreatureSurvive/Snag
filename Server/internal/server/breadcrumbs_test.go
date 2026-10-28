package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestIssuePageShowsBreadcrumbTimeline(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	u := sampleUpload(id1)
	crumbs := []byte(`[
  {"at": "2026-10-27T09:58:55.000Z", "kind": "navigation", "message": "Player", "data": {}},
  {"at": "2026-10-27T09:59:50.000Z", "kind": "network", "message": "GET api.example.com/items/:id → 500 · 80 ms", "data": {"status": "500"}},
  {"at": "2026-10-27T10:00:00.000Z", "kind": "action", "message": "Tapped Play"}
]`)
	u.files["attachment2"] = crumbs
	u.types["attachment2"] = "application/json"
	u.manifest["attachments"] = append(u.manifest["attachments"].([]map[string]any),
		map[string]any{"id": "A2000000-0000-0000-0000-000000000000", "role": "log", "filename": "breadcrumbs.json", "contentType": "application/json", "size": len(crumbs), "part": "attachment2"})
	if rr := post(t, srv, u, testKey); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body)
	}
	page := adminGet(srv, "/p/gelo/issues/1").Body.String()
	for _, want := range []string{"Activity before the report · 3 steps", "−1m 05s", "Player", `class="crumb network failed"`, "−10s", "−now"} {
		if !strings.Contains(page, want) {
			t.Errorf("issue page lacks %q", want)
		}
	}
	if strings.Contains(page, `"kind": "navigation"`) {
		t.Error("breadcrumbs also shown as raw JSON")
	}
}
