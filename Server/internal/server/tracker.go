package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Issue statuses. New, triaged and in progress count as open.
const (
	IssueNew        = "new"
	IssueTriaged    = "triaged"
	IssueInProgress = "in_progress"
	IssueResolved   = "resolved"
	IssueWontFix    = "wontfix"
)

var Statuses = []string{IssueNew, IssueTriaged, IssueInProgress, IssueResolved, IssueWontFix}

func ValidStatus(s string) bool { return contains(Statuses, s) }

func IsOpenStatus(s string) bool { return s == IssueNew || s == IssueTriaged || s == IssueInProgress }

func StatusLabel(s string) string {
	switch s {
	case IssueNew:
		return "New"
	case IssueTriaged:
		return "Triaged"
	case IssueInProgress:
		return "In progress"
	case IssueResolved:
		return "Resolved"
	case IssueWontFix:
		return "Won't fix"
	}
	return s
}

// Priorities, lowest to highest. "" is unset.
var Priorities = []string{"", "low", "medium", "high", "urgent"}

func PriorityRank(p string) int {
	for i, v := range Priorities {
		if v == p {
			return i
		}
	}
	return 0
}

func ValidPriority(p string) bool { return contains(Priorities, p) }

// Issue groups one or more reports about the same problem.
type Issue struct {
	Project     string   `json:"project"`
	Number      int      `json:"number"`
	Title       string   `json:"title"`
	Kind        string   `json:"kind"`
	Status      string   `json:"status"`
	Priority    string   `json:"priority,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Reports     []string `json:"reports"` // report IDs, oldest first
	AppVersions []string `json:"appVersions,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	// FingerprintV is the fingerprint format; older formats are recomputed on startup.
	FingerprintV int `json:"fingerprintV,omitempty"`
	// FirstVersion is the app version of the first report.
	FirstVersion string `json:"firstVersion,omitempty"`
	// FixedIn is the app version the fix ships in. A report from that version or later
	// reopens the issue as a regression.
	FixedIn      string    `json:"fixedIn,omitempty"`
	Regressions  int       `json:"regressions,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	LastReportAt time.Time `json:"lastReportAt"`
	MergedInto   int       `json:"mergedInto,omitempty"`
	Notes        []Note    `json:"notes,omitempty"`
	// GitHub links the issue a report was forwarded to, for two-way sync.
	GitHub *GitHubLink `json:"github,omitempty"`
	// Thread holds messages the reporters see in the app.
	Thread []ThreadMessage `json:"thread,omitempty"`
	// Public puts the issue on the project's ideas board, with PublicNote as its description.
	Public     bool   `json:"public,omitempty"`
	PublicNote string `json:"publicNote,omitempty"`
	// Votes from the board; Voters are hashed voter IDs, so each votes once.
	Votes    int      `json:"votes,omitempty"`
	Voters   []string `json:"voters,omitempty"`
	Activity []Event  `json:"activity,omitempty"`
}

type Note struct {
	ID   string    `json:"id"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

func (i *Issue) IsOpen() bool { return IsOpenStatus(i.Status) && i.MergedInto == 0 }

func (i *Issue) log(kind, format string, args ...any) {
	now := time.Now().UTC()
	i.Activity = append(i.Activity, Event{At: now, Kind: kind, Text: fmt.Sprintf(format, args...)})
	i.UpdatedAt = now
}

func (i *Issue) clone() *Issue {
	data, _ := json.Marshal(i)
	var c Issue
	_ = json.Unmarshal(data, &c)
	return &c
}

// FingerprintVersion is the current Fingerprint format.
const FingerprintVersion = 2

// Fingerprint identifies reports that are word-for-word the same problem, on any app version
// (so a regression in a later version matches the issue it was fixed under).
func Fingerprint(rec *Record) string {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	return rec.Kind + "\x00" + norm(rec.Title) + "\x00" + norm(rec.Message)
}

func reportVersion(rec *Record) string {
	if rec.Context.App == nil {
		return ""
	}
	return rec.Context.App.Version
}

// CompareVersions orders dotted versions numerically ("3.10" > "3.9"); non-numeric parts
// compare as text. It returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	split := func(v string) []string {
		return strings.FieldsFunc(strings.TrimPrefix(strings.TrimSpace(v), "v"), func(r rune) bool { return r == '.' || r == '-' || r == '+' || r == ' ' })
	}
	pa, pb := split(a), split(b)
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y string
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		nx, errx := strconv.Atoi(x)
		ny, erry := strconv.Atoi(y)
		if x == "" {
			nx, errx = 0, nil
		}
		if y == "" {
			ny, erry = 0, nil
		}
		switch {
		// A trailing label is a pre-release: "2.0-beta" < "2.0".
		case errx != nil && y == "":
			return -1
		case erry != nil && x == "":
			return 1
		case errx == nil && erry == nil:
			if nx != ny {
				if nx < ny {
					return -1
				}
				return 1
			}
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return 0
}

// FileResult says what happened to a new report.
type FileResult struct {
	Issue *Issue
	// Created is true when the report opened a new issue.
	Created bool
	// Regressed is true when it reopened a resolved issue.
	Regressed bool
}

// IssueStore keeps one JSON file per issue under <data>/issues/<project>/<number>.json.
type IssueStore struct {
	dir    string
	mu     sync.RWMutex
	issues map[string]map[int]*Issue // project -> number -> issue
	next   map[string]int
}

func OpenIssues(dataDir string) (*IssueStore, error) {
	is := &IssueStore{dir: filepath.Join(dataDir, "issues"), issues: map[string]map[int]*Issue{}, next: map[string]int{}}
	if err := os.MkdirAll(is.dir, 0o750); err != nil {
		return nil, err
	}
	projects, err := os.ReadDir(is.dir)
	if err != nil {
		return nil, err
	}
	for _, pdir := range projects {
		if !pdir.IsDir() || !slugPattern.MatchString(pdir.Name()) {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(is.dir, pdir.Name()))
		for _, f := range files {
			n, err := strconv.Atoi(strings.TrimSuffix(f.Name(), ".json"))
			if err != nil || n <= 0 {
				continue
			}
			data, err := os.ReadFile(filepath.Join(is.dir, pdir.Name(), f.Name()))
			if err != nil {
				continue
			}
			var issue Issue
			if json.Unmarshal(data, &issue) != nil || issue.Number != n {
				continue
			}
			is.put(&issue)
		}
	}
	return is, nil
}

func (is *IssueStore) put(issue *Issue) {
	if is.issues[issue.Project] == nil {
		is.issues[issue.Project] = map[int]*Issue{}
	}
	is.issues[issue.Project][issue.Number] = issue
	if issue.Number >= is.next[issue.Project] {
		is.next[issue.Project] = issue.Number + 1
	}
}

func (is *IssueStore) saveLocked(issue *Issue) error {
	dir := filepath.Join(is.dir, issue.Project)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, strconv.Itoa(issue.Number)+".json"), issue)
}

// File puts a new report into an issue. With group, a report identical to an existing issue
// joins it: an open issue simply gains the report; a resolved one is reopened as a regression
// unless the report comes from a version older than the one the fix shipped in.
func (is *IssueStore) File(project string, rec *Record, group bool) (FileResult, error) {
	is.mu.Lock()
	defer is.mu.Unlock()
	fp := Fingerprint(rec)
	version := reportVersion(rec)
	if group {
		var match *Issue
		for _, issue := range is.issues[project] {
			if issue.MergedInto != 0 || issue.Fingerprint != fp || issue.Status == IssueWontFix {
				continue
			}
			// Prefer an open match, then the most recent resolved one.
			if match == nil || (issue.IsOpen() && !match.IsOpen()) || (issue.IsOpen() == match.IsOpen() && issue.Number > match.Number) {
				match = issue
			}
		}
		if match != nil {
			updated := match.clone()
			addReport(updated, rec, version)
			regressed := false
			switch {
			case updated.IsOpen():
				updated.log("report", "Another identical report arrived")
			case updated.FixedIn != "" && version != "" && CompareVersions(version, updated.FixedIn) < 0:
				updated.log("report", "Report from %s, before the fix in %s", version, updated.FixedIn)
			default:
				regressed = true
				reopenAsRegression(updated, version)
			}
			if err := is.saveLocked(updated); err != nil {
				return FileResult{}, err
			}
			is.put(updated)
			return FileResult{Issue: updated.clone(), Regressed: regressed}, nil
		}
	}
	if is.next[project] == 0 {
		is.next[project] = 1
	}
	now := time.Now().UTC()
	issue := &Issue{
		Project: project, Number: is.next[project],
		Title: reportTitle(rec), Kind: rec.Kind, Status: IssueNew,
		Fingerprint: fp, FingerprintV: FingerprintVersion, FirstVersion: version,
		CreatedAt: now, UpdatedAt: now,
	}
	for _, tag := range rec.Tags {
		if tag = normalizeLabel(tag); tag != "" && !contains(issue.Labels, tag) {
			issue.Labels = append(issue.Labels, tag)
		}
	}
	addReport(issue, rec, version)
	issue.log("created", "Opened from a report")
	if err := is.saveLocked(issue); err != nil {
		return FileResult{}, err
	}
	is.put(issue)
	return FileResult{Issue: issue.clone(), Created: true}, nil
}

func reopenAsRegression(issue *Issue, version string) {
	issue.Regressions++
	from := issue.Status
	issue.Status = IssueNew
	if !contains(issue.Labels, "regression") {
		issue.Labels = append(issue.Labels, "regression")
		sort.Strings(issue.Labels)
	}
	switch {
	case issue.FixedIn != "" && version != "":
		issue.log("regression", "Regressed: reported in %s after the fix in %s (%s → New)", version, issue.FixedIn, StatusLabel(from))
	case version != "":
		issue.log("regression", "Regressed: reported again in %s after being resolved (%s → New)", version, StatusLabel(from))
	default:
		issue.log("regression", "Regressed: reported again after being resolved (%s → New)", StatusLabel(from))
	}
}

// SetFixedIn records the version a fix ships in.
func SetFixedIn(issue *Issue, version string) {
	version = truncate(strings.TrimSpace(version), 40)
	if version == issue.FixedIn {
		return
	}
	if version == "" {
		issue.log("release", "Cleared the fixed-in version")
	} else {
		issue.log("release", "Fixed in %s", version)
	}
	issue.FixedIn = version
}

// Refingerprint updates issues stored with an older fingerprint format, using text to find
// each issue's first report.
func (is *IssueStore) Refingerprint(first func(*Issue) *Record) int {
	is.mu.Lock()
	defer is.mu.Unlock()
	n := 0
	for _, issues := range is.issues {
		for _, issue := range issues {
			if issue.FingerprintV >= FingerprintVersion || issue.MergedInto != 0 {
				continue
			}
			rec := first(issue)
			if rec == nil {
				continue
			}
			updated := issue.clone()
			updated.Fingerprint, updated.FingerprintV = Fingerprint(rec), FingerprintVersion
			if updated.FirstVersion == "" {
				updated.FirstVersion = reportVersion(rec)
			}
			if is.saveLocked(updated) == nil {
				is.put(updated)
				n++
			}
		}
	}
	return n
}

func addReport(issue *Issue, rec *Record, version string) {
	if !contains(issue.Reports, rec.ID) {
		issue.Reports = append(issue.Reports, rec.ID)
	}
	if version != "" && !contains(issue.AppVersions, version) {
		issue.AppVersions = append(issue.AppVersions, version)
	}
	if rec.ReceivedAt.After(issue.LastReportAt) {
		issue.LastReportAt = rec.ReceivedAt
	}
}

// Unfile removes a report from its issue, deleting the issue if it was the only report.
func (is *IssueStore) Unfile(project string, number int, reportID string) error {
	is.mu.Lock()
	defer is.mu.Unlock()
	issue, ok := is.issues[project][number]
	if !ok {
		return ErrNotFound
	}
	updated := issue.clone()
	for i, id := range updated.Reports {
		if id == reportID {
			updated.Reports = append(updated.Reports[:i], updated.Reports[i+1:]...)
			break
		}
	}
	if len(updated.Reports) == 0 && updated.MergedInto == 0 {
		delete(is.issues[project], number)
		return os.Remove(filepath.Join(is.dir, project, strconv.Itoa(number)+".json"))
	}
	updated.log("report", "A report was deleted")
	if err := is.saveLocked(updated); err != nil {
		return err
	}
	is.put(updated)
	return nil
}

func (is *IssueStore) Get(project string, number int) (*Issue, error) {
	is.mu.RLock()
	defer is.mu.RUnlock()
	issue, ok := is.issues[project][number]
	if !ok {
		return nil, ErrNotFound
	}
	return issue.clone(), nil
}

// Update applies fn to a copy of the issue and saves it.
func (is *IssueStore) Update(project string, number int, fn func(*Issue) error) (*Issue, error) {
	is.mu.Lock()
	defer is.mu.Unlock()
	issue, ok := is.issues[project][number]
	if !ok {
		return nil, ErrNotFound
	}
	updated := issue.clone()
	if err := fn(updated); err != nil {
		return nil, err
	}
	if err := is.saveLocked(updated); err != nil {
		return nil, err
	}
	is.put(updated)
	return updated.clone(), nil
}

// SetStatus, SetPriority and the label helpers record what changed in the activity log.
func SetStatus(issue *Issue, status string) error {
	if !ValidStatus(status) {
		return fmt.Errorf("unknown status %q", status)
	}
	if issue.Status != status {
		issue.log("status", "%s → %s", StatusLabel(issue.Status), StatusLabel(status))
		issue.Status = status
	}
	return nil
}

func SetPriority(issue *Issue, priority string) error {
	if !ValidPriority(priority) {
		return fmt.Errorf("unknown priority %q", priority)
	}
	if issue.Priority != priority {
		from, to := issue.Priority, priority
		if from == "" {
			from = "none"
		}
		if to == "" {
			to = "none"
		}
		issue.log("priority", "Priority %s → %s", from, to)
		issue.Priority = priority
	}
	return nil
}

func normalizeLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' || r == ':' || r == '/' {
			return r
		}
		if unicode.IsSpace(r) {
			return '-'
		}
		return -1
	}, s)
	return truncate(s, 40)
}

func AddLabel(issue *Issue, label string) {
	label = normalizeLabel(label)
	if label == "" || contains(issue.Labels, label) {
		return
	}
	issue.Labels = append(issue.Labels, label)
	sort.Strings(issue.Labels)
	issue.log("label", "Added label %s", label)
}

func RemoveLabel(issue *Issue, label string) {
	for i, l := range issue.Labels {
		if l == label {
			issue.Labels = append(issue.Labels[:i], issue.Labels[i+1:]...)
			issue.log("label", "Removed label %s", label)
			return
		}
	}
}

func SetTitle(issue *Issue, title string) error {
	title = truncate(strings.TrimSpace(title), 300)
	if title == "" {
		return errors.New("the title can't be empty")
	}
	if title != issue.Title {
		issue.log("title", "Renamed from “%s”", issue.Title)
		issue.Title = title
	}
	return nil
}

func AddNote(issue *Issue, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("the note is empty")
	}
	issue.Notes = append(issue.Notes, Note{ID: newID(), At: time.Now().UTC(), Text: truncate(text, 20_000)})
	issue.log("note", "Added a note")
	return nil
}

func DeleteNote(issue *Issue, id string) {
	for i, n := range issue.Notes {
		if n.ID == id {
			issue.Notes = append(issue.Notes[:i], issue.Notes[i+1:]...)
			issue.log("note", "Deleted a note")
			return
		}
	}
}

// Merge moves every report, label and note from the `from` issues into `into`. The merged
// issues are kept as redirects so old links still work.
func (is *IssueStore) Merge(project string, into int, from []int) (*Issue, error) {
	is.mu.Lock()
	defer is.mu.Unlock()
	target, ok := is.issues[project][into]
	if !ok || target.MergedInto != 0 {
		return nil, ErrNotFound
	}
	target = target.clone()
	var sources []*Issue
	for _, n := range from {
		if n == into {
			continue
		}
		src, ok := is.issues[project][n]
		if !ok || src.MergedInto != 0 {
			return nil, fmt.Errorf("issue #%d can't be merged", n)
		}
		sources = append(sources, src.clone())
	}
	if len(sources) == 0 {
		return nil, errors.New("choose at least one other issue to merge")
	}
	for _, src := range sources {
		for _, id := range src.Reports {
			if !contains(target.Reports, id) {
				target.Reports = append(target.Reports, id)
			}
		}
		for _, v := range src.AppVersions {
			if !contains(target.AppVersions, v) {
				target.AppVersions = append(target.AppVersions, v)
			}
		}
		for _, l := range src.Labels {
			if !contains(target.Labels, l) {
				target.Labels = append(target.Labels, l)
			}
		}
		for _, v := range src.Voters {
			target.Vote(v, true) // a voter of both counts once
		}
		src.Votes, src.Voters = 0, nil
		for _, n := range src.Notes {
			n.Text = fmt.Sprintf("(from #%d) %s", src.Number, n.Text)
			target.Notes = append(target.Notes, n)
		}
		for _, m := range src.Thread {
			if m.ReportID == "" {
				// A broadcast to the merged issue's reporters stays with them only.
				for _, id := range src.Reports {
					c := m
					c.ID, c.ReportID = newID(), id
					target.Thread = append(target.Thread, c)
				}
				continue
			}
			target.Thread = append(target.Thread, m)
		}
		if src.LastReportAt.After(target.LastReportAt) {
			target.LastReportAt = src.LastReportAt
		}
		if PriorityRank(src.Priority) > PriorityRank(target.Priority) {
			target.Priority = src.Priority
		}
		target.log("merge", "Merged #%d (%s) into this issue", src.Number, src.Title)
		src.Reports = nil
		src.MergedInto = into
		src.log("merge", "Merged into #%d", into)
	}
	sort.Strings(target.Labels)
	sort.Slice(target.Notes, func(i, j int) bool { return target.Notes[i].At.Before(target.Notes[j].At) })
	sort.SliceStable(target.Thread, func(i, j int) bool { return target.Thread[i].At.Before(target.Thread[j].At) })
	for _, src := range sources {
		if err := is.saveLocked(src); err != nil {
			return nil, err
		}
		is.put(src)
	}
	if err := is.saveLocked(target); err != nil {
		return nil, err
	}
	is.put(target)
	return target.clone(), nil
}

// Delete removes an issue file (its reports are deleted by the caller).
func (is *IssueStore) Delete(project string, number int) error {
	is.mu.Lock()
	defer is.mu.Unlock()
	if _, ok := is.issues[project][number]; !ok {
		return ErrNotFound
	}
	delete(is.issues[project], number)
	return os.Remove(filepath.Join(is.dir, project, strconv.Itoa(number)+".json"))
}

// DeleteProject removes every issue of a project.
func (is *IssueStore) DeleteProject(project string) error {
	is.mu.Lock()
	defer is.mu.Unlock()
	delete(is.issues, project)
	delete(is.next, project)
	return os.RemoveAll(filepath.Join(is.dir, project))
}

type IssueFilter struct {
	// Status is a status, "open" (new, triaged, in progress), "closed" (resolved, won't fix) or "" for all.
	Status   string
	Priority string
	Label    string
	Kind     string
	Version  string
	// Regressed keeps only issues that have regressed at least once.
	Regressed bool
	Query     string
	// Sort is "updated" (default), "created", "reports" or "priority".
	Sort   string
	Limit  int
	Offset int
	// Match, when set, decides Query (see ParseSearch); without it only titles and labels are
	// searched.
	Match func(issue *Issue, query SearchQuery) bool
}

func (f IssueFilter) keep(issue *Issue) bool {
	if issue.MergedInto != 0 {
		return false
	}
	switch f.Status {
	case "":
	case "open":
		if !IsOpenStatus(issue.Status) {
			return false
		}
	case "closed":
		if IsOpenStatus(issue.Status) {
			return false
		}
	default:
		if issue.Status != f.Status {
			return false
		}
	}
	if f.Priority != "" && issue.Priority != f.Priority {
		return false
	}
	if f.Label != "" && !contains(issue.Labels, f.Label) {
		return false
	}
	if f.Kind != "" && issue.Kind != f.Kind {
		return false
	}
	if f.Version != "" && !contains(issue.AppVersions, f.Version) && issue.FixedIn != f.Version {
		return false
	}
	if f.Regressed && issue.Regressions == 0 {
		return false
	}
	if q := ParseSearch(f.Query); !q.Empty() {
		if f.Match != nil {
			return f.Match(issue, q)
		}
		text := strings.ToLower(issue.Title + " " + strings.Join(issue.Labels, " "))
		for _, t := range q.Terms {
			if !strings.Contains(text, t) {
				return false
			}
		}
	}
	return true
}

// List returns a project's matching issues and the total count before paging.
func (is *IssueStore) List(project string, f IssueFilter) ([]*Issue, int) {
	is.mu.RLock()
	var out []*Issue
	for _, issue := range is.issues[project] {
		out = append(out, issue)
	}
	is.mu.RUnlock()
	kept := out[:0]
	for _, issue := range out {
		if f.keep(issue) {
			kept = append(kept, issue)
		}
	}
	out = kept
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch f.Sort {
		case "created":
			return a.Number > b.Number
		case "reports":
			if len(a.Reports) != len(b.Reports) {
				return len(a.Reports) > len(b.Reports)
			}
		case "priority":
			if PriorityRank(a.Priority) != PriorityRank(b.Priority) {
				return PriorityRank(a.Priority) > PriorityRank(b.Priority)
			}
		case "votes":
			if a.Score() != b.Score() {
				return a.Score() > b.Score()
			}
		}
		au, bu := a.UpdatedAt, b.UpdatedAt
		if a.LastReportAt.After(au) {
			au = a.LastReportAt
		}
		if b.LastReportAt.After(bu) {
			bu = b.LastReportAt
		}
		if !au.Equal(bu) {
			return au.After(bu)
		}
		return a.Number > b.Number
	})
	total := len(out)
	if f.Offset > 0 {
		if f.Offset >= len(out) {
			out = nil
		} else {
			out = out[f.Offset:]
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	clones := make([]*Issue, len(out))
	for i, issue := range out {
		clones[i] = issue.clone()
	}
	return clones, total
}

// ProjectStats summarizes a project's issues for the overview.
type ProjectStats struct {
	ByStatus  map[string]int
	Open      int
	Urgent    int // open and high or urgent
	NewToday  int // issues created in the last 24 hours
	LastAt    time.Time
	LastTitle string
}

func (is *IssueStore) Stats(project string) ProjectStats {
	is.mu.RLock()
	defer is.mu.RUnlock()
	stats := ProjectStats{ByStatus: map[string]int{}}
	dayAgo := time.Now().Add(-24 * time.Hour)
	for _, issue := range is.issues[project] {
		if issue.MergedInto != 0 {
			continue
		}
		stats.ByStatus[issue.Status]++
		if IsOpenStatus(issue.Status) {
			stats.Open++
			if PriorityRank(issue.Priority) >= PriorityRank("high") {
				stats.Urgent++
			}
		}
		if issue.CreatedAt.After(dayAgo) {
			stats.NewToday++
		}
		if issue.LastReportAt.After(stats.LastAt) {
			stats.LastAt, stats.LastTitle = issue.LastReportAt, issue.Title
		}
	}
	return stats
}

// Facets lists labels, kinds and versions in use, for filter menus.
func (is *IssueStore) Facets(project string) (labels, kinds, versions []string) {
	is.mu.RLock()
	defer is.mu.RUnlock()
	l, k, v := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, issue := range is.issues[project] {
		if issue.MergedInto != 0 {
			continue
		}
		for _, x := range issue.Labels {
			l[x] = true
		}
		k[issue.Kind] = true
		for _, x := range issue.AppVersions {
			v[x] = true
		}
	}
	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for x := range m {
			if x != "" {
				out = append(out, x)
			}
		}
		sort.Strings(out)
		return out
	}
	return keys(l), keys(k), keys(v)
}

// MARK: Similarity

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for that this with when what was are but not you have has had
		its it's from they then them there can cant can't does doesn't did didn't into onto out just get got
		app also any all after before been being will would should could your our very too much more some
		again still only like seems seem keep keeps every time times trying tried try`) {
		stopwords[w] = true
	}
}

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) >= 3 && !stopwords[w] {
			out[w] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// Similar is a likely duplicate and how alike it is (0–1).
type Similar struct {
	Issue *Issue
	Score float64
}

// Similar ranks the project's other unmerged issues by how alike they are to issue. text maps
// an issue to the words to compare (title plus report messages).
func (is *IssueStore) Similar(issue *Issue, text func(*Issue) string, limit int) []Similar {
	is.mu.RLock()
	var candidates []*Issue
	for _, other := range is.issues[issue.Project] {
		if other.Number != issue.Number && other.MergedInto == 0 {
			candidates = append(candidates, other.clone())
		}
	}
	is.mu.RUnlock()
	base := tokens(text(issue))
	titleBase := tokens(issue.Title)
	var out []Similar
	for _, other := range candidates {
		score := 0.6*jaccard(base, tokens(text(other))) + 0.4*jaccard(titleBase, tokens(other.Title))
		if other.Kind == issue.Kind {
			score += 0.05
		}
		for _, v := range other.AppVersions {
			if contains(issue.AppVersions, v) {
				score += 0.05
				break
			}
		}
		if !other.IsOpen() {
			score *= 0.8
		}
		if score >= 0.2 {
			out = append(out, Similar{Issue: other, Score: min(score, 1)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Count is the number of issues across all projects.
func (is *IssueStore) Count() int {
	is.mu.RLock()
	defer is.mu.RUnlock()
	n := 0
	for _, issues := range is.issues {
		n += len(issues)
	}
	return n
}

// Current returns an issue, following merges to the issue that absorbed it.
func (is *IssueStore) Current(project string, number int) (*Issue, error) {
	issue, err := is.Get(project, number)
	for hops := 0; err == nil && issue.MergedInto != 0 && hops < 10; hops++ {
		issue, err = is.Get(project, issue.MergedInto)
	}
	return issue, err
}
