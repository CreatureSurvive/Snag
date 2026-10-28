package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestIdeasBoard(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	post(t, srv, uploadWith(ids(3)[0], "Dark mode", "Please add dark mode. I'm alice@example.com"), testKey)
	post(t, srv, uploadWith(ids(3)[1], "Offline downloads", "secret message"), testKey)
	post(t, srv, uploadWith(ids(3)[2], "Crash on launch", "it crashes"), testKey)

	get := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	if rr := get("/b/gelo", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("board before it's enabled: %d", rr.Code)
	}
	adminPost(t, srv, "/p/gelo/settings", url.Values{"name": {"Gelo"}, "boardEnabled": {"on"}, "boardIntro": {"Vote for what's next."}})
	adminPost(t, srv, "/p/gelo/issues/1/update", url.Values{"board": {"1"}, "public": {"on"}, "publicNote": {"A dark theme for the whole app."}})
	adminPost(t, srv, "/p/gelo/issues/2/update", url.Values{"board": {"1"}, "public": {"on"}, "status": {"in_progress"}})

	rr := get("/b/gelo", nil)
	page := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(page, "Dark mode") || !strings.Contains(page, "A dark theme for the whole app.") || !strings.Contains(page, "In progress") {
		t.Fatalf("board: %d\n%s", rr.Code, page)
	}
	for _, private := range []string{"Crash on launch", "alice@example.com", "secret message", "csrf"} {
		if strings.Contains(page, private) {
			t.Errorf("public board shows %q", private)
		}
	}
	if strings.Index(page, "Offline downloads") > strings.Index(page, "Dark mode") {
		t.Error("in-progress ideas should come first")
	}
	cookie := rr.Result().Cookies()[0]
	token := regexp.MustCompile(`name="token" value="([0-9a-f]+)"`).FindStringSubmatch(page)[1]

	vote := func(n string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/b/gelo/ideas/"+n+"/vote", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	vote("1", url.Values{"token": {token}, "vote": {"1"}}, cookie)
	vote("1", url.Values{"token": {token}, "vote": {"1"}}, cookie) // twice counts once
	vote("1", url.Values{"token": {"forged"}, "vote": {"1"}}, nil) // no cookie, bad token
	vote("3", url.Values{"token": {token}, "vote": {"1"}}, cookie) // not public
	issue, _ := srv.issues.Get("gelo", 1)
	if issue.Votes != 1 || issue.Score() != 2 {
		t.Fatalf("votes %d score %d", issue.Votes, issue.Score())
	}
	if three, _ := srv.issues.Get("gelo", 3); three.Votes != 0 {
		t.Fatal("voted on an unpublished issue")
	}
	if !strings.Contains(get("/b/gelo", cookie).Body.String(), `aria-pressed="true"`) {
		t.Error("the visitor's vote isn't shown as pressed")
	}

	// The app's API: list, vote with a device ID, unvote.
	api := func(method, path string, body []byte, voter string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Content-Type", "application/json")
		if voter != "" {
			req.Header.Set("X-Snag-Voter", voter)
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	device := "device-0123456789abcdef"
	rr = api("POST", "/api/v1/board/1/vote", []byte(`{"vote": true}`), device)
	var item BoardItem
	_ = json.Unmarshal(rr.Body.Bytes(), &item)
	if rr.Code != 200 || item.Votes != 3 || !item.Voted {
		t.Fatalf("app vote: %d %s", rr.Code, rr.Body)
	}
	var board struct {
		Title string      `json:"title"`
		Intro string      `json:"intro"`
		Items []BoardItem `json:"items"`
	}
	_ = json.Unmarshal(api("GET", "/api/v1/board", nil, device).Body.Bytes(), &board)
	if board.Title != "Gelo ideas" || board.Intro != "Vote for what's next." || len(board.Items) != 2 || !board.Items[1].Voted {
		t.Fatalf("board API: %+v", board)
	}
	api("POST", "/api/v1/board/1/vote", []byte(`{"vote": false}`), device)
	if issue, _ := srv.issues.Get("gelo", 1); issue.Votes != 1 {
		t.Fatalf("after unvote: %d", issue.Votes)
	}
	if rr := api("POST", "/api/v1/board/3/vote", nil, device); rr.Code != http.StatusNotFound {
		t.Fatalf("unpublished via API: %d", rr.Code)
	}

	// Merging moves the votes.
	adminPost(t, srv, "/p/gelo/issues/2/merge", url.Values{"from": {"1"}})
	if two, _ := srv.issues.Get("gelo", 2); two.Votes != 1 {
		t.Fatalf("merged votes: %d", two.Votes)
	}
}
