package server

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestRedactorMatchesClientRules(t *testing.T) {
	r := NewRedactor(RedactStandard)
	cases := map[string]string{
		"Authorization: Bearer abcdefghijklmnop1234":   "Authorization: Bearer [redacted:bearer]",
		"password=hunter2&user=me":                     "password=[redacted:secret]&user=me",
		`{"api_key": "sk_live_123456"}`:                `{"api_key": "[redacted:secret]"}`,
		"see https://bob:pa55@example.com/x":           "see https://[redacted:credentials]@example.com/x",
		"card 4242 4242 4242 4242 ok":                  "card [redacted:card] ok",
		"order 1234567890123 isn't a card":             "order 1234567890123 isn't a card",
		"jwt eyJhbGciOi.eyJzdWIiOiIx.c2lnbmF0dXJl end": "jwt [redacted:jwt] end",
		"mail me@example.com":                          "mail me@example.com",
		"Bearer [redacted:bearer]":                     "Bearer [redacted:bearer]",
		"password=[redacted:secret]":                   "password=[redacted:secret]",
	}
	for in, want := range cases {
		if got := r.Text(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	if r.Removed != 6 {
		t.Errorf("removed %d, want 6", r.Removed)
	}

	strict := NewRedactor(RedactStrict)
	got := strict.Text("me@example.com from 10.0.0.12, call +1 555-123-4567, build 7 fe80:0:0:0:0:0:0:1")
	want := "[redacted:email] from [redacted:ip], call [redacted:phone], build 7 [redacted:ip]"
	if got != want {
		t.Errorf("strict: got %q, want %q", got, want)
	}
	if off := NewRedactor(RedactOff); off.Text("password=x123") != "password=x123" {
		t.Error("off should not redact")
	}
}

func TestIngestRedactsWhatTheAppMissed(t *testing.T) {
	srv, store := newTestServer(t, testConfig(t))
	u := sampleUpload(id1)
	u.manifest["message"] = "my password=hunter22"
	u.files["attachment1"] = []byte("GET /x\nAuthorization: Bearer abcdefghijklmnop1234\n")
	if rr := post(t, srv, u, testKey); rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	rec, err := store.Get(strings.ToLower(id1))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Message != "my password=[redacted:secret]" || rec.ServerRedactions != 2 {
		t.Fatalf("message %q, redactions %d", rec.Message, rec.ServerRedactions)
	}
	if rec.Contact != "me@example.com" {
		t.Errorf("contact changed: %q", rec.Contact)
	}
	var log StoredFile
	for _, f := range rec.Files {
		if f.Role == "log" {
			log = f
		}
	}
	path, file, err := store.FilePath(rec.ID, log.Name)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "abcdefghijklmnop1234") || int64(len(data)) != file.Size {
		t.Fatalf("log not redacted or size stale: %q (size %d)", data, file.Size)
	}
}
