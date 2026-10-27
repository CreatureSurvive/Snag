package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHubLink connects a Snag issue to the GitHub issue it was forwarded to.
type GitHubLink struct {
	Repo   string `json:"repo"` // owner/repo
	Number int    `json:"number"`
	URL    string `json:"url"`
	// State is GitHub's view: "open" or "closed".
	State     string    `json:"state,omitempty"`
	SyncedAt  time.Time `json:"syncedAt,omitempty"`
	LastError string    `json:"lastError,omitempty"`
	// Labels is the label set last seen on GitHub, to tell remote changes from ours.
	Labels []string `json:"labels,omitempty"`
	// Comments already copied into notes, by GitHub comment ID.
	Comments []int64 `json:"comments,omitempty"`
}

var githubIssueURL = regexp.MustCompile(`^https://github\.com/([^/]+/[^/]+)/issues/(\d+)$`)

// GitHubChange is what changed in Snag and should be mirrored on GitHub.
type GitHubChange struct {
	Status       bool
	AddLabels    []string
	RemoveLabels []string
	Comment      string
}

func (c GitHubChange) empty() bool {
	return !c.Status && len(c.AddLabels) == 0 && len(c.RemoveLabels) == 0 && c.Comment == ""
}

// GitHubSync mirrors issue state between Snag and GitHub: status, labels and priority go out;
// closes, reopens, labels and comments come back (by webhook, or by polling without one).
type GitHubSync struct {
	issues   *IssueStore
	projects *ProjectStore
	cfg      Config
	log      *slog.Logger
	wg       sync.WaitGroup
	mu       sync.Mutex
	// Per-issue locks so pushes for one issue apply in order.
	locks map[string]*sync.Mutex
}

func NewGitHubSync(issues *IssueStore, projects *ProjectStore, cfg Config, logger *slog.Logger) *GitHubSync {
	return &GitHubSync{issues: issues, projects: projects, cfg: cfg, log: logger, locks: map[string]*sync.Mutex{}}
}

func (g *GitHubSync) client(p *Project) *GitHubForwarder {
	f := p.Forwarding.GitHub
	if f == nil || f.Repo == "" {
		return nil
	}
	owner, repo, _ := strings.Cut(f.Repo, "/")
	token := f.Token
	if token == "" {
		token = g.cfg.GitHubToken
	}
	if token == "" {
		return nil
	}
	api := g.cfg.GitHubAPIURL
	if api == "" {
		api = "https://api.github.com"
	}
	return &GitHubForwarder{cfg: GitHubConfig{Token: token, Owner: owner, Repo: repo, Labels: f.Labels, APIURL: api}}
}

func (g *GitHubSync) lock(key string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.locks[key] == nil {
		g.locks[key] = &sync.Mutex{}
	}
	return g.locks[key]
}

// Link records the GitHub issue a report was forwarded to.
func (g *GitHubSync) Link(project string, number int, issueURL string) {
	m := githubIssueURL.FindStringSubmatch(issueURL)
	if m == nil {
		return
	}
	n, _ := strconv.Atoi(m[2])
	_, _ = g.issues.Update(project, number, func(i *Issue) error {
		if i.GitHub == nil {
			i.GitHub = &GitHubLink{Repo: m[1], Number: n, URL: issueURL, State: "open", SyncedAt: time.Now().UTC()}
			i.log("github", "Linked to GitHub %s#%d", m[1], n)
		}
		return nil
	})
}

// Wait blocks until in-flight pushes finish.
func (g *GitHubSync) Wait() { g.wg.Wait() }

// Push mirrors a change to the linked GitHub issue in the background.
func (g *GitHubSync) Push(project string, number int, change GitHubChange) {
	if change.empty() {
		return
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := g.push(ctx, project, number, change); err != nil {
			g.log.Warn("github sync failed", "project", project, "issue", number, "err", err)
		}
	}()
}

func githubPriorityLabel(p string) string {
	if p == "" {
		return ""
	}
	return "priority: " + p
}

func (g *GitHubSync) push(ctx context.Context, project string, number int, change GitHubChange) error {
	l := g.lock(fmt.Sprintf("%s/%d", project, number))
	l.Lock()
	defer l.Unlock()
	p, ok := g.projects.Get(project)
	if !ok {
		return nil
	}
	issue, err := g.issues.Get(project, number)
	if err != nil || issue.GitHub == nil {
		return nil
	}
	client := g.client(p)
	if client == nil || !strings.EqualFold(issue.GitHub.Repo, client.cfg.Owner+"/"+client.cfg.Repo) {
		return nil
	}
	path := fmt.Sprintf("/repos/%s/issues/%d", issue.GitHub.Repo, issue.GitHub.Number)
	var failure error
	note := func(err error) {
		if err != nil && failure == nil {
			failure = err
		}
	}
	state := issue.GitHub.State
	if change.Status {
		body := map[string]any{"state": "open"}
		if !IsOpenStatus(issue.Status) || issue.MergedInto != 0 {
			body["state"] = "closed"
			body["state_reason"] = "completed"
			if issue.Status == IssueWontFix || issue.MergedInto != 0 {
				body["state_reason"] = "not_planned"
			}
		} else if issue.GitHub.State == "closed" {
			body["state_reason"] = "reopened"
		}
		if body["state"] != issue.GitHub.State || body["state"] == "closed" {
			_, err := client.api(ctx, "PATCH", path, body)
			note(err)
			if err == nil {
				state = body["state"].(string)
			}
		}
	}
	if len(change.AddLabels) > 0 {
		_, err := client.api(ctx, "POST", path+"/labels", map[string]any{"labels": change.AddLabels})
		note(err)
	}
	for _, label := range change.RemoveLabels {
		_, err := client.api(ctx, "DELETE", path+"/labels/"+url.PathEscape(label), nil)
		if err != nil && strings.Contains(err.Error(), "HTTP 404") {
			err = nil // already gone
		}
		note(err)
	}
	if change.Comment != "" {
		_, err := client.api(ctx, "POST", path+"/comments", map[string]any{"body": snagCommentMarker + "\n" + change.Comment})
		note(err)
	}
	_, _ = g.issues.Update(project, number, func(i *Issue) error {
		if i.GitHub == nil {
			return nil
		}
		// What we pushed is now GitHub's state, not a remote change to import later.
		for _, l := range change.AddLabels {
			if !containsFold(i.GitHub.Labels, l) {
				i.GitHub.Labels = append(i.GitHub.Labels, l)
			}
		}
		for _, l := range change.RemoveLabels {
			i.GitHub.Labels = removeFold(i.GitHub.Labels, l)
		}
		i.GitHub.State = state
		i.GitHub.SyncedAt = time.Now().UTC()
		i.GitHub.LastError = ""
		if failure != nil {
			i.GitHub.LastError = truncate(failure.Error(), 300)
		}
		return nil
	})
	return failure
}

// ignoredLabels are GitHub labels that Snag adds itself and shouldn't import back.
func ignoredLabels(p *Project, issue *Issue) map[string]bool {
	out := map[string]bool{strings.ToLower(issue.Kind): true}
	if p.Forwarding.GitHub != nil {
		for _, l := range p.Forwarding.GitHub.Labels {
			out[strings.ToLower(l)] = true
		}
	}
	return out
}

// applyRemote updates a Snag issue from GitHub's state and labels. It returns the change
// description for logs, or "" if nothing changed.
func applyRemote(p *Project, issue *Issue, state, stateReason string, labels []string, closedBy string) {
	if issue.GitHub == nil {
		return
	}
	who := ""
	if closedBy != "" {
		who = " by @" + closedBy
	}
	switch {
	case state == "closed" && IsOpenStatus(issue.Status):
		to := IssueResolved
		if stateReason == "not_planned" {
			to = IssueWontFix
		}
		issue.log("github", "Closed on GitHub%s", who)
		issue.Status = to
	case state == "open" && !IsOpenStatus(issue.Status) && issue.GitHub.State == "closed":
		issue.log("github", "Reopened on GitHub%s", who)
		issue.Status = IssueTriaged
	}
	if state != "" {
		issue.GitHub.State = state
	}
	if labels != nil {
		// Only differences from the last known GitHub labels are changes made on GitHub.
		ignored := ignoredLabels(p, issue)
		for _, l := range labels {
			if containsFold(issue.GitHub.Labels, l) {
				continue
			}
			lower := strings.ToLower(l)
			switch {
			case strings.HasPrefix(lower, "priority: "):
				if v := strings.TrimPrefix(lower, "priority: "); ValidPriority(v) && v != issue.Priority {
					issue.log("github", "Priority set to %s on GitHub", v)
					issue.Priority = v
				}
			case !ignored[lower]:
				AddLabel(issue, l)
			}
		}
		for _, l := range issue.GitHub.Labels {
			if containsFold(labels, l) {
				continue
			}
			lower := strings.ToLower(l)
			switch {
			case strings.HasPrefix(lower, "priority: "):
				if strings.TrimPrefix(lower, "priority: ") == issue.Priority {
					issue.log("github", "Priority cleared on GitHub")
					issue.Priority = ""
				}
			case !ignored[lower]:
				RemoveLabel(issue, normalizeLabel(l))
			}
		}
		issue.GitHub.Labels = append([]string{}, labels...)
	}
	issue.GitHub.SyncedAt = time.Now().UTC()
}

func importComment(issue *Issue, id int64, author, body string) bool {
	for _, seen := range issue.GitHub.Comments {
		if seen == id {
			return false
		}
	}
	// Comments Snag posted itself start with this marker.
	if strings.HasPrefix(body, snagCommentMarker) {
		issue.GitHub.Comments = append(issue.GitHub.Comments, id)
		return false
	}
	issue.GitHub.Comments = append(issue.GitHub.Comments, id)
	if len(issue.GitHub.Comments) > 500 {
		issue.GitHub.Comments = issue.GitHub.Comments[len(issue.GitHub.Comments)-500:]
	}
	_ = AddNote(issue, fmt.Sprintf("GitHub comment by @%s:\n%s", author, body))
	return true
}

const snagCommentMarker = "<!-- snag -->"

// find locates the Snag issue linked to a GitHub issue.
func (g *GitHubSync) find(repo string, number int) (*Project, *Issue) {
	for _, p := range g.projects.List() {
		issues, _ := g.issues.List(p.Slug, IssueFilter{})
		for _, i := range issues {
			if i.GitHub != nil && i.GitHub.Number == number && strings.EqualFold(i.GitHub.Repo, repo) {
				return p, i
			}
		}
	}
	return nil, nil
}

// HandleWebhook applies GitHub "issues" and "issue_comment" events. Configure it on the
// repository at <public URL>/api/v1/github/webhook with SNAG_GITHUB_WEBHOOK_SECRET.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	secret := s.cfg.GitHubWebhookSecret
	if secret == "" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		fail(w, http.StatusBadRequest, "unreadable body")
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Hub-Signature-256"))) {
		fail(w, http.StatusUnauthorized, "bad signature")
		return
	}
	var event struct {
		Action string `json:"action"`
		Issue  struct {
			Number      int    `json:"number"`
			State       string `json:"state"`
			StateReason string `json:"state_reason"`
			Labels      []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"issue"`
		Comment *struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"user"`
		} `json:"comment"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	kind := r.Header.Get("X-GitHub-Event")
	if kind == "ping" {
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	p, issue := s.github.find(event.Repository.FullName, event.Issue.Number)
	if issue == nil {
		writeJSONResponse(w, http.StatusOK, map[string]any{"ignored": "not linked"})
		return
	}
	labels := make([]string, 0, len(event.Issue.Labels))
	for _, l := range event.Issue.Labels {
		labels = append(labels, l.Name)
	}
	_, err = s.issues.Update(p.Slug, issue.Number, func(i *Issue) error {
		switch {
		case kind == "issues" && (event.Action == "closed" || event.Action == "reopened"):
			applyRemote(p, i, event.Issue.State, event.Issue.StateReason, nil, event.Sender.Login)
		case kind == "issues" && (event.Action == "labeled" || event.Action == "unlabeled"):
			applyRemote(p, i, "", "", labels, "")
		case kind == "issue_comment" && event.Action == "created" && event.Comment != nil && event.Comment.User.Type != "Bot":
			importComment(i, event.Comment.ID, event.Comment.User.Login, event.Comment.Body)
		}
		return nil
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "update failed")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// Poll refreshes linked, recently active issues from GitHub, for servers GitHub can't reach.
func (g *GitHubSync) Poll(ctx context.Context) {
	cutoff := time.Now().Add(-90 * 24 * time.Hour)
	for _, p := range g.projects.List() {
		client := g.client(p)
		if client == nil {
			continue
		}
		issues, _ := g.issues.List(p.Slug, IssueFilter{})
		for _, issue := range issues {
			if ctx.Err() != nil {
				return
			}
			if issue.GitHub == nil || issue.UpdatedAt.Before(cutoff) && !issue.IsOpen() {
				continue
			}
			g.pollOne(ctx, p, client, issue)
		}
	}
}

func (g *GitHubSync) pollOne(ctx context.Context, p *Project, client *GitHubForwarder, issue *Issue) {
	path := fmt.Sprintf("/repos/%s/issues/%d", issue.GitHub.Repo, issue.GitHub.Number)
	data, err := client.api(ctx, "GET", path, nil)
	if err != nil {
		return
	}
	var remote struct {
		State       string `json:"state"`
		StateReason string `json:"state_reason"`
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
		ClosedBy *struct {
			Login string `json:"login"`
		} `json:"closed_by"`
	}
	if json.Unmarshal(data, &remote) != nil {
		return
	}
	var comments []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	}
	since := issue.GitHub.SyncedAt.Add(-time.Hour).UTC().Format(time.RFC3339)
	if data, err := client.api(ctx, "GET", path+"/comments?per_page=100&since="+url.QueryEscape(since), nil); err == nil {
		_ = json.Unmarshal(data, &comments)
	}
	labels := make([]string, 0, len(remote.Labels))
	for _, l := range remote.Labels {
		labels = append(labels, l.Name)
	}
	closedBy := ""
	if remote.ClosedBy != nil {
		closedBy = remote.ClosedBy.Login
	}
	_, _ = g.issues.Update(p.Slug, issue.Number, func(i *Issue) error {
		applyRemote(p, i, remote.State, remote.StateReason, labels, closedBy)
		for _, c := range comments {
			if c.User.Type != "Bot" {
				importComment(i, c.ID, c.User.Login, c.Body)
			}
		}
		return nil
	})
}

func containsFold(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(item, v) {
			return true
		}
	}
	return false
}

func removeFold(list []string, v string) []string {
	out := list[:0]
	for _, item := range list {
		if !strings.EqualFold(item, v) {
			out = append(out, item)
		}
	}
	return out
}
