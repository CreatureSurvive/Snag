package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The ideas board: a public page per project listing the issues you choose to publish, with
// votes from visitors and from the app (SnagBoard). Reporters' messages and contacts are never
// shown; a published issue shows its title, status and the public note you write.

// BoardConfig turns on a project's public board at /b/<slug>.
type BoardConfig struct {
	Enabled bool   `json:"enabled"`
	Title   string `json:"title,omitempty"`
	Intro   string `json:"intro,omitempty"`
}

const maxVoters = 10_000

// Score ranks ideas: votes plus the reports asking for the same thing.
func (i *Issue) Score() int { return i.Votes + len(i.Reports) }

// PublicStatus is what visitors see for an issue's status.
func (i *Issue) PublicStatus() (state, label string) {
	switch i.Status {
	case IssueTriaged:
		return "planned", "Planned"
	case IssueInProgress:
		return "in_progress", "In progress"
	case IssueResolved:
		if i.FixedIn != "" {
			return "done", "Done in " + i.FixedIn
		}
		return "done", "Done"
	case IssueWontFix:
		return "closed", "Closed"
	}
	return "open", "Under consideration"
}

// Vote adds or removes one voter's vote. voter is already hashed.
func (i *Issue) Vote(voter string, on bool) bool {
	for n, v := range i.Voters {
		if v == voter {
			if !on {
				i.Voters = append(i.Voters[:n], i.Voters[n+1:]...)
				i.Votes--
			}
			return true
		}
	}
	if !on {
		return true
	}
	if len(i.Voters) >= maxVoters {
		return false
	}
	i.Voters = append(i.Voters, voter)
	i.Votes++
	return true
}

func (i *Issue) VotedBy(voter string) bool {
	for _, v := range i.Voters {
		if v == voter {
			return true
		}
	}
	return false
}

// voterHash turns a visitor cookie or device ID into what's stored, scoped to the project.
func (s *Server) voterHash(project, voter string) string {
	mac := hmac.New(sha256.New, s.boardKey())
	mac.Write([]byte(project + "|" + voter))
	return hex.EncodeToString(mac.Sum(nil)[:12])
}

// boardKey is kept in <data>/board.key so voter hashes stay stable across restarts.
func (s *Server) boardKey() []byte { return s.boardSecret }

func loadOrCreateKey(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil && len(data) >= 32 {
		return data, nil
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// BoardItem is one idea as the public sees it.
type BoardItem struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Note   string `json:"note,omitempty"`
	State  string `json:"state"`
	Status string `json:"status"`
	Votes  int    `json:"votes"`
	Voted  bool   `json:"voted"`
}

func (s *Server) boardItems(project, voter string) []BoardItem {
	issues, _ := s.issues.List(project, IssueFilter{})
	var out []BoardItem
	for _, i := range issues {
		if !i.Public || i.MergedInto != 0 {
			continue
		}
		state, label := i.PublicStatus()
		out = append(out, BoardItem{Number: i.Number, Title: i.Title, Note: i.PublicNote, State: state, Status: label,
			Votes: i.Score(), Voted: voter != "" && i.VotedBy(voter)})
	}
	rank := map[string]int{"in_progress": 0, "planned": 1, "open": 2, "done": 3, "closed": 4}
	sort.SliceStable(out, func(a, b int) bool {
		if rank[out[a].State] != rank[out[b].State] {
			return rank[out[a].State] < rank[out[b].State]
		}
		if out[a].Votes != out[b].Votes {
			return out[a].Votes > out[b].Votes
		}
		return out[a].Number > out[b].Number
	})
	return out
}

func (s *Server) publicProject(w http.ResponseWriter, r *http.Request) (*Project, bool) {
	p, ok := s.projects.Get(r.PathValue("slug"))
	if !ok || !p.Board.Enabled {
		http.NotFound(w, r)
		return nil, false
	}
	return p, true
}

const voterCookie = "snag_voter"

// visitor returns the browser's voter ID, setting the cookie on first visit.
func visitor(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(voterCookie); err == nil && len(c.Value) == 32 {
		return c.Value
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: voterCookie, Value: id, Path: "/b/", MaxAge: 5 * 365 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
	return id
}

// voteToken ties the vote form to the visitor's cookie, so other sites can't vote for them.
func (s *Server) voteToken(voter string) string {
	mac := hmac.New(sha256.New, s.csrfKey)
	mac.Write([]byte("vote|" + voter))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	p, ok := s.publicProject(w, r)
	if !ok {
		return
	}
	voter := visitor(w, r)
	title := p.Board.Title
	if title == "" {
		title = p.Name + " ideas"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", dashboardCSP)
	w.Header().Set("Cache-Control", "no-store")
	data := map[string]any{"P": p, "Title": title, "Items": s.boardItems(p.Slug, s.voterHash(p.Slug, voter)),
		"Token": s.voteToken(voter), "Flash": r.URL.Query().Get("flash")}
	if err := templates.ExecuteTemplate(w, "board.html", data); err != nil {
		s.log.Error("rendering board", "err", err)
	}
}

func (s *Server) handleBoardVote(w http.ResponseWriter, r *http.Request) {
	p, ok := s.publicProject(w, r)
	if !ok {
		return
	}
	voter := visitor(w, r)
	if subtle.ConstantTimeCompare([]byte(r.FormValue("token")), []byte(s.voteToken(voter))) != 1 {
		http.Redirect(w, r, "/b/"+p.Slug, http.StatusSeeOther)
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	if err := s.castVote(p, n, s.voterHash(p.Slug, voter), r.FormValue("vote") != "0", s.clientIP(r)); err != nil {
		http.Redirect(w, r, "/b/"+p.Slug+"?flash="+err.Error(), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/b/"+p.Slug+"#idea-"+strconv.Itoa(n), http.StatusSeeOther)
}

var errNoIdea = errors.New("no such idea")

func (s *Server) castVote(p *Project, number int, voter string, on bool, ip string) error {
	if !s.limiter.allow("vote|" + p.Slug + "|" + ip) {
		return errors.New("too many votes from this address; try again later")
	}
	_, err := s.issues.Update(p.Slug, number, func(i *Issue) error {
		if !i.Public || i.MergedInto != 0 {
			return errNoIdea
		}
		if !i.Vote(voter, on) {
			return errors.New("this idea can't take more votes")
		}
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return errNoIdea
	}
	return err
}

// The app's board API, authenticated with the ingest key. X-Snag-Voter is a random ID the app
// keeps per install.

func (s *Server) boardProject(w http.ResponseWriter, r *http.Request) (*Project, string, bool) {
	p, ok := s.authenticateProject(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "missing or invalid API key")
		return nil, "", false
	}
	if !p.Board.Enabled {
		fail(w, http.StatusNotFound, "this project has no ideas board")
		return nil, "", false
	}
	voter := strings.TrimSpace(r.Header.Get("X-Snag-Voter"))
	if len(voter) < 16 || len(voter) > 64 {
		voter = ""
	}
	if voter != "" {
		voter = s.voterHash(p.Slug, "app|"+voter)
	}
	return p, voter, true
}

func (s *Server) handleAPIBoard(w http.ResponseWriter, r *http.Request) {
	p, voter, ok := s.boardProject(w, r)
	if !ok {
		return
	}
	title := p.Board.Title
	if title == "" {
		title = p.Name + " ideas"
	}
	items := s.boardItems(p.Slug, voter)
	if items == nil {
		items = []BoardItem{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"title": title, "intro": p.Board.Intro, "items": items})
}

func (s *Server) handleAPIBoardVote(w http.ResponseWriter, r *http.Request) {
	p, voter, ok := s.boardProject(w, r)
	if !ok {
		return
	}
	if voter == "" {
		fail(w, http.StatusBadRequest, "send X-Snag-Voter")
		return
	}
	var body struct {
		Vote *bool `json:"vote"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body)
	on := body.Vote == nil || *body.Vote
	n, _ := strconv.Atoi(r.PathValue("n"))
	if err := s.castVote(p, n, voter, on, s.clientIP(r)); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errNoIdea) {
			status = http.StatusNotFound
		}
		fail(w, status, "%v", err)
		return
	}
	for _, item := range s.boardItems(p.Slug, voter) {
		if item.Number == n {
			writeJSONResponse(w, http.StatusOK, item)
			return
		}
	}
	fail(w, http.StatusNotFound, "no such idea")
}

// SetPublic publishes or unpublishes an issue on the board, with the note visitors see.
func SetPublic(i *Issue, public bool, note string) {
	note = truncate(strings.TrimSpace(note), 2000)
	if public != i.Public {
		if public {
			i.log("board", "Published on the ideas board")
		} else {
			i.log("board", "Removed from the ideas board")
		}
	} else if note != i.PublicNote {
		i.log("board", "Updated the public note")
	}
	i.Public, i.PublicNote = public, note
	i.UpdatedAt = time.Now().UTC()
}
