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

func inboundPost(srv *Server, query, contentType string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/inbound-email"+query, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func TestEmailRepliesJoinTheConversation(t *testing.T) {
	cfg := testConfig(t)
	cfg.SMTP = &SMTPConfig{Host: "smtp.example.com", Port: 587, From: "Gelo <support@example.com>", ReplyTo: "Gelo Support <support@example.com>", TLS: "starttls"}
	cfg.InboundEmailSecret = "inbound-secret-for-tests"
	sent := stubSMTP(t)
	srv, _ := newTestServer(t, cfg)
	post(t, srv, sampleUpload(id1), testKey)
	id := strings.ToLower(id1)

	adminPost(t, srv, "/p/gelo/issues/1/reply", url.Values{"report": {id}, "subject": {"Re: Playback"}, "body": {"Does 2.1 fix it?"}})
	head, _, _ := strings.Cut(string(sent()[0].msg), "\r\n\r\n")
	replyTo := regexp.MustCompile(`Reply-To: (.+)\r\n`).FindStringSubmatch(head)
	if replyTo == nil || !strings.Contains(replyTo[1], "support+r"+strings.ReplaceAll(id, "-", "")) {
		t.Fatalf("Reply-To not tagged:\n%s", head)
	}
	tagged := regexp.MustCompile(`support\+r[0-9a-f]{40}@example.com`).FindString(replyTo[1])

	raw := "From: Me <me@example.com>\r\nTo: " + tagged + "\r\nSubject: Re: Playback\r\nMessage-ID: <m1@mail.example.com>\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=b1\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Yes, it's fixed =E2=80=94 thanks! My password=3Dhunter22 btw\r\n\r\nOn Tue, Oct 27, 2026 at 10:00 Gelo <support@example.com> wrote:\r\n> Does 2.1 fix it?\r\n" +
		"--b1\r\nContent-Type: text/html\r\n\r\n<p>Yes</p>\r\n--b1--\r\n"
	if rr := inboundPost(srv, "?key=wrong", "message/rfc822", []byte(raw)); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d", rr.Code)
	}
	rr := inboundPost(srv, "?key=inbound-secret-for-tests", "message/rfc822", []byte(raw))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"accepted":true`) {
		t.Fatalf("inbound: %d %s", rr.Code, rr.Body)
	}
	// A redelivery of the same message isn't added again.
	inboundPost(srv, "?key=inbound-secret-for-tests", "message/rfc822", []byte(raw))
	issue, _ := srv.issues.Get("gelo", 1)
	if len(issue.Thread) != 2 {
		t.Fatalf("thread %+v", issue.Thread)
	}
	team, reply := issue.Thread[0], issue.Thread[1]
	if team.From != FromTeam || team.Via != "email" || team.Text != "Does 2.1 fix it?" {
		t.Errorf("outgoing email not in thread: %+v", team)
	}
	if reply.From != FromReporter || reply.Via != "email" || reply.Text != "Yes, it's fixed — thanks! My password=[redacted:secret] btw" {
		t.Errorf("reply %+v", reply)
	}
	if !strings.Contains(adminGet(srv, "/p/gelo/issues/1").Body.String(), "by email") {
		t.Error("issue page doesn't mark email messages")
	}

	// A forged tag is ignored; so is a References match from someone other than the reporter.
	forged := strings.Replace(raw, tagged, "support+r"+strings.ReplaceAll(id, "-", "")+"00000000@example.com", 1)
	forged = strings.Replace(forged, "<m1@", "<m2@", 1)
	if rr := inboundPost(srv, "?key=inbound-secret-for-tests", "message/rfc822", []byte(forged)); strings.Contains(rr.Body.String(), `"accepted":true`) {
		t.Fatal("forged tag accepted")
	}
	stranger := "From: x@evil.com\r\nTo: support@example.com\r\nReferences: <" + id + "@snag>\r\nMessage-ID: <m3@x>\r\n\r\nspam\r\n"
	if rr := inboundPost(srv, "?key=inbound-secret-for-tests", "message/rfc822", []byte(stranger)); strings.Contains(rr.Body.String(), `"accepted":true`) {
		t.Fatal("stranger accepted through References")
	}

	// Mailgun's form post, matched by References and the reporter's own address.
	form := url.Values{"sender": {"me@example.com"}, "recipient": {"support@example.com"}, "subject": {"Re"},
		"body-plain": {"full text"}, "stripped-text": {"Second answer"}, "message-headers": {`[["Message-Id","<m4@mg>"],["References","<` + id + `@snag>"]]`}}
	if rr := inboundPost(srv, "?key=inbound-secret-for-tests", "application/x-www-form-urlencoded", []byte(form.Encode())); !strings.Contains(rr.Body.String(), `"accepted":true`) {
		t.Fatalf("mailgun: %s", rr.Body)
	}
	// Postmark's JSON, with HTTP basic auth carrying the key.
	pm, _ := json.Marshal(map[string]any{"From": "me@example.com", "To": tagged, "Subject": "Re", "TextBody": "Third\n\nOn Mon someone wrote:\n> old",
		"Headers": []map[string]string{{"Name": "Message-ID", "Value": "<m5@pm>"}}})
	req := httptest.NewRequest("POST", "/api/v1/inbound-email", bytes.NewReader(pm))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("postmark", "inbound-secret-for-tests")
	prr := httptest.NewRecorder()
	srv.ServeHTTP(prr, req)
	if !strings.Contains(prr.Body.String(), `"accepted":true`) {
		t.Fatalf("postmark: %s", prr.Body)
	}
	issue, _ = srv.issues.Get("gelo", 1)
	if n := len(issue.Thread); n != 4 || issue.Thread[2].Text != "Second answer" || issue.Thread[3].Text != "Third" {
		t.Fatalf("thread %+v", issue.Thread)
	}
}

func TestStripQuotedReply(t *testing.T) {
	cases := map[string]string{
		"Thanks!\n\nOn Tue, Oct 27, 2026 at 10:00 AM Dev <d@x.com> wrote:\n> hi":  "Thanks!",
		"Danke\n\nAm 27.10.2026 um 10:00 schrieb Dev <d@x.com>:\n> hi":            "Danke",
		"ok\n-----Original Message-----\nFrom: d":                                 "ok",
		"line one\n> quoted\nline two\n-- \nSent from my phone":                   "line one\nline two",
		"Merci\n\nLe mar. 27 oct. 2026 à 10:00, Dev <d@x.com> a écrit :\n> salut": "Merci",
	}
	for in, want := range cases {
		if got := stripQuotedReply(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
