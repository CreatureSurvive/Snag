package server

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
)

// Email replies from reporters. Replies the dashboard sends carry a Reply-To such as
// support+r<report id><mac>@example.com; whatever receives that mailbox (Cloudflare Email
// Routing, Mailgun, Postmark, SendGrid...) posts the message to /api/v1/inbound-email, and the
// reporter's answer joins the issue's conversation.

const maxInboundBytes = 2 << 20

// replyAddress is the Reply-To for an email about one report: SNAG_SMTP_REPLY_TO with a
// plus-tag that identifies the report and can't be forged without SNAG_INBOUND_EMAIL_SECRET.
func (s *Server) replyAddress(reportID string) string {
	base := ""
	if s.cfg.SMTP != nil {
		base = s.cfg.SMTP.ReplyTo
	}
	if base == "" || s.cfg.InboundEmailSecret == "" {
		return base
	}
	addr, err := mail.ParseAddress(base)
	if err != nil {
		return base
	}
	local, domain, ok := strings.Cut(addr.Address, "@")
	if !ok {
		return base
	}
	local, _, _ = strings.Cut(local, "+")
	tagged := local + "+" + s.replyTag(reportID) + "@" + domain
	if addr.Name != "" {
		return (&mail.Address{Name: addr.Name, Address: tagged}).String()
	}
	return tagged
}

func (s *Server) replyTag(reportID string) string {
	id := strings.ReplaceAll(strings.ToLower(reportID), "-", "")
	mac := hmac.New(sha256.New, []byte(s.cfg.InboundEmailSecret))
	mac.Write([]byte("reply|" + id))
	return "r" + id + hex.EncodeToString(mac.Sum(nil)[:4])
}

var replyTagPattern = regexp.MustCompile(`\+r([0-9a-f]{32})([0-9a-f]{8})@`)

// reportFromTag returns the report ID in a tagged recipient address, if its MAC checks out.
func (s *Server) reportFromTag(addresses ...string) string {
	for _, a := range addresses {
		for _, m := range replyTagPattern.FindAllStringSubmatch(strings.ToLower(a), -1) {
			if subtle.ConstantTimeCompare([]byte("r"+m[1]+m[2]), []byte(s.replyTag(m[1]))) == 1 {
				id := m[1]
				return id[0:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:32]
			}
		}
	}
	return ""
}

// InboundEmail is a received message, whatever format it arrived in.
type InboundEmail struct {
	From       string
	Recipients []string
	Subject    string
	Text       string
	MessageID  string
	References string
}

// handleInboundEmail accepts a raw message (message/rfc822), Postmark's JSON, or Mailgun's and
// SendGrid's form posts. Authenticate with ?key=<SNAG_INBOUND_EMAIL_SECRET> or a Bearer header.
func (s *Server) handleInboundEmail(w http.ResponseWriter, r *http.Request) {
	if s.cfg.InboundEmailSecret == "" {
		fail(w, http.StatusNotFound, "inbound email isn't enabled (set SNAG_INBOUND_EMAIL_SECRET)")
		return
	}
	key := r.URL.Query().Get("key")
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		key = strings.TrimSpace(bearer)
	}
	if _, pass, ok := r.BasicAuth(); ok && key == "" {
		key = pass // Postmark's https://user:secret@host/... form
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.InboundEmailSecret)) != 1 {
		fail(w, http.StatusUnauthorized, "bad inbound email key")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxInboundBytes)
	msg, err := parseInbound(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	result, err := s.acceptEmailReply(msg)
	if err != nil {
		// 200 so providers don't retry a message that will never match.
		s.log.Info("inbound email ignored", "reason", err)
		writeJSONResponse(w, http.StatusOK, map[string]any{"accepted": false, "reason": err.Error()})
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"accepted": true, "issue": result})
}

var errNoReport = errors.New("not a reply to a Snag report")

// acceptEmailReply adds a reporter's email to the conversation of the report it answers.
func (s *Server) acceptEmailReply(msg *InboundEmail) (int, error) {
	id := s.reportFromTag(msg.Recipients...)
	trusted := id != ""
	if id == "" {
		// A reply that lost the tag (some clients reply to From): match References, but only
		// when it comes from the address the reporter gave.
		if m := regexp.MustCompile(`<([0-9a-fA-F-]{36})@snag>`).FindStringSubmatch(msg.References); m != nil {
			id = m[1]
		}
	}
	if id == "" {
		return 0, errNoReport
	}
	rec, err := s.store.Get(id)
	if err != nil {
		return 0, errNoReport
	}
	from, _ := mail.ParseAddress(msg.From)
	if !trusted {
		if from == nil || rec.Contact == "" || !strings.EqualFold(from.Address, strings.TrimSpace(rec.Contact)) {
			return 0, errors.New("the sender isn't the reporter, and the reply address has no valid tag")
		}
	}
	issue, err := s.issues.Current(rec.Project, rec.Issue)
	if err != nil {
		return 0, err
	}
	text := stripQuotedReply(msg.Text)
	if text == "" {
		return 0, errors.New("the reply is empty")
	}
	text = truncate(text, maxReporterText)
	text = NewRedactor(projectRedaction(s, rec.Project)).Text(text)
	duplicate := false
	updated, err := s.issues.Update(issue.Project, issue.Number, func(i *Issue) error {
		if msg.MessageID != "" {
			for _, m := range i.Thread {
				if m.ExternalID == msg.MessageID {
					duplicate = true
					return nil
				}
			}
		}
		if err := AddThreadMessage(i, FromReporter, rec.ID, text); err != nil {
			return err
		}
		last := &i.Thread[len(i.Thread)-1]
		last.Via, last.ExternalID = "email", msg.MessageID
		i.Activity[len(i.Activity)-1].Text = "The reporter replied by email"
		return nil
	})
	if err != nil {
		return 0, err
	}
	if !duplicate {
		if p, ok := s.projects.Get(updated.Project); ok {
			s.notifier.Notify(p, updated.Kind, Alert{
				Event: EventReport, Title: fmt.Sprintf("%s #%d · reporter replied by email", p.Name, updated.Number),
				Message: truncate(text, 500), URL: s.issueURL(p.Slug, updated.Number),
			})
		}
	}
	return updated.Number, nil
}

func projectRedaction(s *Server, slug string) string {
	if p, ok := s.projects.Get(slug); ok {
		return p.Redaction
	}
	return RedactStandard
}

func parseInbound(r *http.Request) (*InboundEmail, error) {
	ct, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case ct == "application/json":
		// Postmark
		var pm struct {
			From, To, Cc, Subject, TextBody, StrippedTextReply, OriginalRecipient, MessageID string
			Headers                                                                          []struct{ Name, Value string }
		}
		if err := json.NewDecoder(r.Body).Decode(&pm); err != nil {
			return nil, fmt.Errorf("JSON: %w", err)
		}
		msg := &InboundEmail{From: pm.From, Subject: pm.Subject, Text: pm.TextBody, Recipients: []string{pm.To, pm.Cc, pm.OriginalRecipient}}
		if pm.StrippedTextReply != "" {
			msg.Text = pm.StrippedTextReply
		}
		for _, h := range pm.Headers {
			switch strings.ToLower(h.Name) {
			case "message-id":
				msg.MessageID = h.Value
			case "references", "in-reply-to":
				msg.References += " " + h.Value
			}
		}
		if msg.MessageID == "" {
			msg.MessageID = pm.MessageID
		}
		return msg, nil
	case ct == "multipart/form-data" || ct == "application/x-www-form-urlencoded":
		if ct == "multipart/form-data" {
			if err := r.ParseMultipartForm(maxInboundBytes); err != nil {
				return nil, err
			}
		} else if err := r.ParseForm(); err != nil {
			return nil, err
		}
		f := r.Form
		// SendGrid with "POST the raw, full MIME message".
		if raw := f.Get("email"); raw != "" {
			return parseRFC822(strings.NewReader(raw))
		}
		msg := &InboundEmail{
			From:       first(f.Get("from"), f.Get("sender")),
			Subject:    f.Get("subject"),
			Text:       first(f.Get("stripped-text"), f.Get("body-plain"), f.Get("text")),
			Recipients: []string{f.Get("recipient"), f.Get("to"), f.Get("To"), f.Get("cc")},
			MessageID:  f.Get("Message-Id"),
			References: f.Get("References") + " " + f.Get("In-Reply-To"),
		}
		// Mailgun's message-headers: [["Name", "Value"], ...]
		var headers [][2]string
		if json.Unmarshal([]byte(f.Get("message-headers")), &headers) == nil {
			for _, h := range headers {
				switch strings.ToLower(h[0]) {
				case "message-id":
					msg.MessageID = h[1]
				case "references", "in-reply-to":
					msg.References += " " + h[1]
				}
			}
		}
		// SendGrid's parsed form lists headers as one text block.
		if hs := f.Get("headers"); hs != "" {
			if parsed, err := mail.ReadMessage(strings.NewReader(hs + "\r\n\r\n")); err == nil {
				msg.MessageID = first(msg.MessageID, parsed.Header.Get("Message-Id"))
				msg.References += " " + parsed.Header.Get("References") + " " + parsed.Header.Get("In-Reply-To")
			}
		}
		_ = params
		return msg, nil
	default:
		// Raw RFC 822 (Cloudflare Email Workers: fetch(url, {body: message.raw})).
		return parseRFC822(r.Body)
	}
}

func first(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func parseRFC822(r io.Reader) (*InboundEmail, error) {
	m, err := mail.ReadMessage(bufio.NewReader(r))
	if err != nil {
		return nil, fmt.Errorf("not an email message: %w", err)
	}
	dec := new(mime.WordDecoder)
	subject, err := dec.DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		subject = m.Header.Get("Subject")
	}
	text, err := plainText(m.Header, m.Body, 0)
	if err != nil {
		return nil, err
	}
	return &InboundEmail{
		From: m.Header.Get("From"), Subject: subject, Text: text,
		Recipients: []string{m.Header.Get("To"), m.Header.Get("Cc"), m.Header.Get("Delivered-To"), m.Header.Get("X-Original-To")},
		MessageID:  m.Header.Get("Message-Id"),
		References: m.Header.Get("References") + " " + m.Header.Get("In-Reply-To"),
	}, nil
}

type headerGetter interface{ Get(string) string }

// plainText finds the text/plain body of a message, descending into multipart parts.
func plainText(h headerGetter, body io.Reader, depth int) (string, error) {
	if depth > 5 {
		return "", errors.New("message nests too deeply")
	}
	ct, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		ct = "text/plain"
	}
	if strings.HasPrefix(ct, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		var html string
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", err
			}
			pct, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if strings.HasPrefix(pct, "multipart/") || pct == "text/plain" || pct == "" {
				text, err := plainText(part.Header, part, depth+1)
				if err == nil && strings.TrimSpace(text) != "" {
					return text, nil
				}
			} else if pct == "text/html" && html == "" {
				b, _ := io.ReadAll(decodeTransfer(part.Header.Get("Content-Transfer-Encoding"), part))
				html = string(b)
			}
		}
		if html != "" {
			return htmlToText(html), nil
		}
		return "", nil
	}
	b, err := io.ReadAll(io.LimitReader(decodeTransfer(h.Get("Content-Transfer-Encoding"), body), maxInboundBytes))
	if err != nil {
		return "", err
	}
	if ct == "text/html" {
		return htmlToText(string(b)), nil
	}
	return string(b), nil
}

func decodeTransfer(encoding string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, &stripSpace{r: r})
	}
	return r
}

// stripSpace drops line breaks inside base64 bodies.
type stripSpace struct{ r io.Reader }

func (s *stripSpace) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	out := p[:0]
	for _, c := range p[:n] {
		if c != '\r' && c != '\n' && c != ' ' && c != '\t' {
			out = append(out, c)
		}
	}
	return len(out), err
}

var (
	htmlTags   = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]+>`)
	htmlBreaks = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>`)
)

func htmlToText(html string) string {
	html = htmlBreaks.ReplaceAllString(html, "\n")
	text := htmlTags.ReplaceAllString(html, "")
	return strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(text)
}

var (
	quoteHeader = regexp.MustCompile(`(?m)^\s*(On .{4,200}wrote:|Am .{4,200}schrieb .{0,100}:|Le .{4,200}a écrit\s?:|El .{4,200}escribió:|-{2,}\s*Original Message\s*-{2,}|_{10,}|From: .+)\s*$`)
	signature   = regexp.MustCompile(`(?m)^-- ?$`)
)

// stripQuotedReply keeps what the reporter wrote: it cuts the quoted original ("On ... wrote:",
// "> " lines) and the signature.
func stripQuotedReply(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if loc := quoteHeader.FindStringIndex(text); loc != nil {
		text = text[:loc[0]]
	}
	if loc := signature.FindStringIndex(text); loc != nil {
		text = text[:loc[0]]
	}
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue
		}
		kept = append(kept, strings.TrimRight(line, " \t"))
	}
	return strings.TrimSpace(string(bytes.TrimSpace([]byte(strings.Join(kept, "\n")))))
}
