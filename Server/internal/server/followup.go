package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ThreadMessage is a message between you and a reporter, shown in the app's "My Reports".
// Unlike notes, reporters see these.
type ThreadMessage struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
	// From is "team" or "reporter".
	From string `json:"from"`
	// ReportID addresses one reporter; "" means every reporter of the issue.
	ReportID string `json:"reportId,omitempty"`
	Text     string `json:"text"`
	// Via is "email" for messages sent or received by email; "" is the app.
	Via string `json:"via,omitempty"`
	// ExternalID is an email's Message-ID, so a redelivered message isn't added twice.
	ExternalID string `json:"externalId,omitempty"`
}

const (
	FromTeam     = "team"
	FromReporter = "reporter"

	maxThreadMessages = 200
	maxReporterText   = 5000
)

// newReaderToken returns a token for the app to read its report's status, and its hash.
func newReaderToken() (token, hash string) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	token = "snagr_" + hex.EncodeToString(b)
	return token, hashKey(token)
}

// AddThreadMessage appends a message and logs it.
func AddThreadMessage(issue *Issue, from, reportID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("the message is empty")
	}
	if len(issue.Thread) >= maxThreadMessages {
		return errors.New("this conversation has too many messages")
	}
	issue.Thread = append(issue.Thread, ThreadMessage{ID: newID(), At: time.Now().UTC(), From: from, ReportID: reportID, Text: truncate(text, 20_000)})
	switch {
	case from == FromReporter:
		issue.log("thread", "The reporter replied in the app")
	case reportID == "":
		issue.log("thread", "Messaged every reporter in the app")
	default:
		issue.log("thread", "Messaged the reporter in the app")
	}
	return nil
}

// ThreadFor returns the messages one reporter can see, oldest first.
func (i *Issue) ThreadFor(reportID string) []ThreadMessage {
	var out []ThreadMessage
	for _, m := range i.Thread {
		if m.ReportID == "" || m.ReportID == reportID {
			out = append(out, m)
		}
	}
	return out
}

// ReporterStatus is what the app shows for a report's issue.
type ReporterStatus struct {
	ReportID  string          `json:"reportId"`
	Status    string          `json:"status"` // received, in_progress, fixed, closed
	Label     string          `json:"label"`
	FixedIn   string          `json:"fixedIn,omitempty"`
	UpdatedAt time.Time       `json:"updatedAt"`
	Messages  []ThreadMessage `json:"messages"`
}

func reporterStatus(issue *Issue, reportID string) ReporterStatus {
	st := ReporterStatus{ReportID: reportID, UpdatedAt: issue.UpdatedAt, FixedIn: issue.FixedIn, Messages: issue.ThreadFor(reportID)}
	switch issue.Status {
	case IssueInProgress:
		st.Status, st.Label = "in_progress", "Being worked on"
	case IssueResolved:
		st.Status, st.Label = "fixed", "Fixed"
		if issue.FixedIn != "" {
			st.Label = "Fixed in " + issue.FixedIn
		}
	case IssueWontFix:
		st.Status, st.Label = "closed", "Closed"
	default:
		st.Status, st.Label = "received", "Received"
	}
	if st.Messages == nil {
		st.Messages = []ThreadMessage{}
	}
	// Reporters don't see whether a message was addressed to them alone, or email details.
	for i := range st.Messages {
		st.Messages[i].ReportID = ""
		st.Messages[i].ExternalID = ""
	}
	return st
}

// followUpRecord authenticates a reader token for a report and returns the report and its
// current issue (following merges).
func (s *Server) followUpRecord(w http.ResponseWriter, r *http.Request) (*Record, *Issue, bool) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	rec, err := s.store.Get(r.PathValue("id"))
	if err != nil || rec.ReaderTokenHash == "" || subtle.ConstantTimeCompare([]byte(hashKey(strings.TrimSpace(token))), []byte(rec.ReaderTokenHash)) != 1 {
		// Same answer for unknown reports and wrong tokens; guessing is limited per address.
		if !s.limiter.allow("followup-miss|" + s.clientIP(r)) {
			w.Header().Set("Retry-After", "600")
			fail(w, http.StatusTooManyRequests, "too many requests")
			return nil, nil, false
		}
		fail(w, http.StatusNotFound, "not found")
		return nil, nil, false
	}
	// Valid requests are limited per report, not per address: one app checks many reports, and
	// many users can share an address.
	if !s.limiter.allow("followup|" + rec.ID) {
		w.Header().Set("Retry-After", "600")
		fail(w, http.StatusTooManyRequests, "too many requests")
		return nil, nil, false
	}
	issue, err := s.issues.Current(rec.Project, rec.Issue)
	if err != nil {
		fail(w, http.StatusNotFound, "not found")
		return nil, nil, false
	}
	return rec, issue, true
}

func (s *Server) handleFollowUp(w http.ResponseWriter, r *http.Request) {
	rec, issue, ok := s.followUpRecord(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResponse(w, http.StatusOK, reporterStatus(issue, rec.ID))
}

func (s *Server) handleFollowUpReply(w http.ResponseWriter, r *http.Request) {
	rec, issue, ok := s.followUpRecord(w, r)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "expected {\"text\": \"…\"}")
		return
	}
	text := strings.TrimSpace(body.Text)
	if len(text) > maxReporterText {
		fail(w, http.StatusRequestEntityTooLarge, "messages are limited to %d characters", maxReporterText)
		return
	}
	updated, err := s.issues.Update(issue.Project, issue.Number, func(i *Issue) error {
		return AddThreadMessage(i, FromReporter, rec.ID, text)
	})
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if p, ok := s.projects.Get(updated.Project); ok {
		s.notifier.Notify(p, updated.Kind, Alert{
			Event: EventReport, Title: fmt.Sprintf("%s #%d · reporter replied", p.Name, updated.Number),
			Message: truncate(text, 500), URL: s.issueURL(p.Slug, updated.Number),
		})
	}
	writeJSONResponse(w, http.StatusOK, reporterStatus(updated, rec.ID))
}

// handleAPIThreadMessage posts a team message from a script: {"text": "…", "report": "<id>"}.
// It requires a JSON body, which a cross-site form can't send.
func (s *Server) handleAPIThreadMessage(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, http.StatusUnsupportedMediaType, "send application/json")
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	issue, err := s.issues.Get(r.PathValue("slug"), n)
	if err != nil {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	var body struct {
		Text   string `json:"text"`
		Report string `json:"report"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	report := strings.ToLower(body.Report)
	if report != "" && !contains(issue.Reports, report) {
		fail(w, http.StatusBadRequest, "that report isn't part of this issue")
		return
	}
	updated, err := s.issues.Update(issue.Project, issue.Number, func(i *Issue) error {
		return AddThreadMessage(i, FromTeam, report, body.Text)
	})
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"thread": updated.Thread})
}

// handleThreadMessage posts a team message from the dashboard.
func (s *Server) handleThreadMessage(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	issue, ok := s.issueFrom(w, r)
	if !ok {
		return
	}
	reportID := r.FormValue("report")
	if reportID != "" && !contains(issue.Reports, strings.ToLower(reportID)) {
		http.Error(w, "unknown report", http.StatusBadRequest)
		return
	}
	if _, err := s.issues.Update(p.Slug, issue.Number, func(i *Issue) error {
		return AddThreadMessage(i, FromTeam, strings.ToLower(reportID), r.FormValue("text"))
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	anchor := "#thread"
	if reportID != "" {
		anchor = "#report-" + reportID
	}
	http.Redirect(w, r, issuePath(p.Slug, issue.Number)+anchor, http.StatusSeeOther)
}
