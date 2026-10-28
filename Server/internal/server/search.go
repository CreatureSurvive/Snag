package server

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Search: every word (or "quoted phrase") must appear somewhere in the issue: its title,
// labels, notes and conversation, or its reports' text, contacts, versions, devices and custom
// context. "in:logs" also searches text attachments (logs, breadcrumbs, crash logs).

// SearchQuery is a parsed search.
type SearchQuery struct {
	Terms  []string // lowercased
	InLogs bool
	Number int // "#12" alone jumps to issue 12
}

func ParseSearch(q string) SearchQuery {
	var out SearchQuery
	var b strings.Builder
	quoted := false
	flush := func() {
		term := strings.ToLower(strings.TrimSpace(b.String()))
		b.Reset()
		switch {
		case term == "":
		case term == "in:logs":
			out.InLogs = true
		default:
			out.Terms = append(out.Terms, term)
		}
	}
	for _, r := range q {
		switch {
		case r == '"':
			flush()
			quoted = !quoted
		case unicode.IsSpace(r) && !quoted:
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	if len(out.Terms) == 1 && strings.HasPrefix(out.Terms[0], "#") {
		out.Number, _ = strconv.Atoi(out.Terms[0][1:])
	}
	return out
}

func (q SearchQuery) Empty() bool { return len(q.Terms) == 0 }

// issueText is what an issue itself contributes to search.
func issueText(i *Issue) string {
	parts := []string{i.Title, i.Kind, i.Status, i.Priority, i.FixedIn, i.PublicNote, "#" + strconv.Itoa(i.Number)}
	parts = append(parts, i.Labels...)
	parts = append(parts, i.AppVersions...)
	for _, n := range i.Notes {
		parts = append(parts, n.Text)
	}
	for _, m := range i.Thread {
		parts = append(parts, m.Text)
	}
	return strings.ToLower(strings.Join(parts, "\n"))
}

func (r *Record) searchText() string {
	fields := []string{r.ID, r.Title, r.Message, r.Contact, r.Kind, strings.Join(r.Tags, " ")}
	if r.Context.App != nil {
		fields = append(fields, r.Context.App.Name, r.Context.App.Version, r.Context.App.Build, r.Context.App.BundleIdentifier)
	}
	for k, v := range r.Context.Device {
		if s, ok := v.(string); ok && (k == "model" || k == "system" || k == "systemVersion" || k == "locale") {
			fields = append(fields, s)
		}
	}
	for _, v := range r.Context.Custom {
		fields = append(fields, v)
	}
	return strings.ToLower(strings.Join(fields, "\n"))
}

// textCache keeps lowercased text attachments for "in:logs", bounded in memory.
type textCache struct {
	mu    sync.Mutex
	items map[string]string
	order []string
	size  int
}

const (
	textCacheLimit   = 64 << 20
	textCachePerFile = 512 << 10
)

func (c *textCache) get(key string, load func() (string, error)) string {
	c.mu.Lock()
	if v, ok := c.items[key]; ok {
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()
	v, err := load()
	if err != nil {
		return ""
	}
	if len(v) > textCachePerFile {
		v = v[len(v)-textCachePerFile:] // the end of a log is what matters most
	}
	v = strings.ToLower(v)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]string{}
	}
	if _, ok := c.items[key]; !ok {
		c.items[key] = v
		c.order = append(c.order, key)
		c.size += len(v)
		for c.size > textCacheLimit && len(c.order) > 0 {
			old := c.order[0]
			c.order = c.order[1:]
			c.size -= len(c.items[old])
			delete(c.items, old)
		}
	}
	return v
}

// matchIssue reports whether every term appears in the issue or its reports.
func (s *Server) matchIssue(issue *Issue, q SearchQuery) bool {
	if q.Number != 0 {
		return issue.Number == q.Number
	}
	remaining := append([]string{}, q.Terms...)
	check := func(text string) bool {
		kept := remaining[:0]
		for _, t := range remaining {
			if !strings.Contains(text, t) {
				kept = append(kept, t)
			}
		}
		remaining = kept
		return len(remaining) == 0
	}
	if check(issueText(issue)) {
		return true
	}
	records := s.store.ForIssue(issue.Reports)
	for _, rec := range records {
		if check(rec.searchText()) {
			return true
		}
	}
	if !q.InLogs {
		return false
	}
	for _, rec := range records {
		for _, f := range rec.Files {
			if !f.IsText() {
				continue
			}
			id, name := rec.ID, f.Name
			text := s.textIndex.get(id+"/"+name, func() (string, error) {
				path, _, err := s.store.FilePath(id, name)
				if err != nil {
					return "", err
				}
				data, err := os.ReadFile(path)
				return string(data), err
			})
			if check(text) {
				return true
			}
		}
	}
	return false
}
