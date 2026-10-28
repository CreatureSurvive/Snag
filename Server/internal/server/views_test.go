package server

import (
	"encoding/csv"
	"net/url"
	"strings"
	"testing"
)

func TestSearchSavedViewsAndCSV(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	post(t, srv, uploadWith(ids(3)[0], "Playback stalls on Wi-Fi", "The video freezes after a minute"), testKey)
	post(t, srv, uploadWith(ids(3)[1], "=HYPERLINK(\"http://evil\")", "Login fails with timeout"), testKey)
	post(t, srv, uploadWith(ids(3)[2], "Settings crash", "open settings, app quits"), testKey)

	list := func(q string) string { return adminGet(srv, "/p/gelo?q="+url.QueryEscape(q)).Body.String() }
	if page := list("video wi-fi"); !strings.Contains(page, "Playback stalls") || strings.Contains(page, "Settings crash") {
		t.Error("two words across title and message should find only #1")
	}
	if page := list(`"app quits"`); !strings.Contains(page, "Settings crash") || strings.Contains(page, "Playback") {
		t.Error("phrase search")
	}
	if page := list("video settings"); strings.Contains(page, "Playback") || strings.Contains(page, "Settings crash") {
		t.Error("every word must match")
	}
	// Log text is only searched with in:logs (the sample upload's log says "line 1").
	if strings.Contains(list("line"), "Playback") {
		t.Error("logs searched without in:logs")
	}
	if !strings.Contains(list("line in:logs"), "Playback") {
		t.Error("in:logs didn't search the log")
	}
	if page := adminGet(srv, "/search?q=timeout").Body.String(); !strings.Contains(page, "Gelo") || !strings.Contains(page, "#2") {
		t.Errorf("global search:\n%s", page)
	}

	adminPost(t, srv, "/p/gelo/views", url.Values{"name": {"Crashes"}, "query": {"q=crash&status=all&page=3&junk=1"}})
	p, _ := srv.Projects().Get("gelo")
	if len(p.Views) != 1 || p.Views[0].Query != "q=crash&status=all" {
		t.Fatalf("views %+v", p.Views)
	}
	if !strings.Contains(adminGet(srv, "/p/gelo").Body.String(), `>Crashes</a>`) {
		t.Error("saved view not shown")
	}
	adminPost(t, srv, "/p/gelo/views/"+p.Views[0].ID+"/delete", nil)
	if p, _ := srv.Projects().Get("gelo"); len(p.Views) != 0 {
		t.Fatal("view not deleted")
	}

	rr := adminGet(srv, "/p/gelo/issues.csv?status=all&sort=created")
	if !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("csv type %s", rr.Header().Get("Content-Type"))
	}
	rows, err := csv.NewReader(strings.NewReader(rr.Body.String())).ReadAll()
	if err != nil || len(rows) != 4 || rows[0][0] != "number" {
		t.Fatalf("csv: %v %v", err, rows)
	}
	if rows[2][1] != `'=HYPERLINK("http://evil")` {
		t.Errorf("formula not neutralized: %q", rows[2][1])
	}
}
