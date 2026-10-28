package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Project is one app (or family of apps) with its own keys, issues and forwarding.
type Project struct {
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	// Keys created in the dashboard. Only hashes are stored.
	Keys       []APIKey          `json:"keys,omitempty"`
	Forwarding ProjectForwarding `json:"forwarding"`
	Notify     NotifyConfig      `json:"notify"`
	Attest     AttestConfig      `json:"attest"`
	// GroupDuplicates files a report with identical title and message (and the same app
	// version) under the existing open issue instead of opening a new one.
	GroupDuplicates bool `json:"groupDuplicates"`
	// Redaction is the server-side scrub level: "" (standard), "strict" or "off".
	Redaction string `json:"redaction,omitempty"`
	// FromConfig marks projects seeded from SNAG_API_KEYS or the projects file.
	FromConfig bool `json:"fromConfig,omitempty"`

	// configKeys are hashes of keys from the environment or projects file; never stored.
	configKeys []string
}

type APIKey struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Prefix    string    `json:"prefix"` // first characters, to recognize a key
	Hash      string    `json:"hash"`   // sha256 hex
	CreatedAt time.Time `json:"createdAt"`
}

// ProjectForwarding is where a project's new issues are sent.
type ProjectForwarding struct {
	GitHub  *ProjectGitHub `json:"github,omitempty"`
	Discord string         `json:"discord,omitempty"`
	Slack   string         `json:"slack,omitempty"`
	Webhook *WebhookConfig `json:"webhook,omitempty"`
	Email   []string       `json:"email,omitempty"` // recipients; uses the server's SMTP settings
}

type ProjectGitHub struct {
	Repo             string   `json:"repo"` // owner/repo
	Labels           []string `json:"labels,omitempty"`
	AttachmentBranch string   `json:"attachmentBranch,omitempty"`
	// Token overrides SNAG_GITHUB_TOKEN for this project.
	Token string `json:"token,omitempty"`
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Slugify turns a name into a URL-safe project slug.
func Slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// NewKey returns a fresh ingest key: "snag_" + 40 hex characters.
func NewKey() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return "snag_" + hex.EncodeToString(b)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ProjectStore keeps projects in <data>/projects.json.
type ProjectStore struct {
	path     string
	mu       sync.RWMutex
	projects map[string]*Project
}

// ProjectSeed is a project from the environment or the projects file.
type ProjectSeed struct {
	Slug            string            `json:"slug"`
	Name            string            `json:"name"`
	Keys            []string          `json:"keys"`
	Forwarding      ProjectForwarding `json:"forwarding"`
	Notify          *NotifyConfig     `json:"notify,omitempty"`
	Attest          *AttestConfig     `json:"attest,omitempty"`
	GroupDuplicates *bool             `json:"groupDuplicates,omitempty"`
	Redaction       string            `json:"redaction,omitempty"`
}

var ErrProjectExists = errors.New("a project with that slug already exists")

// OpenProjects loads stored projects and merges in seeds: seed keys are always accepted; the
// rest of a seed only applies when the project doesn't exist yet.
func OpenProjects(dataDir string, seeds []ProjectSeed) (*ProjectStore, error) {
	ps := &ProjectStore{path: filepath.Join(dataDir, "projects.json"), projects: map[string]*Project{}}
	if data, err := os.ReadFile(ps.path); err == nil {
		var list []*Project
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("projects.json: %w", err)
		}
		for _, p := range list {
			ps.projects[p.Slug] = p
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	changed := false
	for _, seed := range seeds {
		slug := seed.Slug
		if slug == "" {
			slug = Slugify(seed.Name)
		}
		if !slugPattern.MatchString(slug) {
			return nil, fmt.Errorf("project %q: slug must be lowercase letters, digits and dashes", seed.Name)
		}
		p, ok := ps.projects[slug]
		if !ok {
			name := seed.Name
			if name == "" {
				name = slug
			}
			p = &Project{Slug: slug, Name: name, CreatedAt: time.Now().UTC(), Forwarding: seed.Forwarding, FromConfig: true}
			if seed.GroupDuplicates != nil {
				p.GroupDuplicates = *seed.GroupDuplicates
			}
			p.Redaction = seed.Redaction
			if seed.Notify != nil {
				p.Notify = *seed.Notify
			}
			if seed.Attest != nil {
				p.Attest = *seed.Attest
			}
			ps.projects[slug] = p
			changed = true
		}
		for _, key := range seed.Keys {
			p.configKeys = append(p.configKeys, hashKey(key))
		}
	}
	// A key may belong to one project only.
	owner := map[string]string{}
	for _, p := range ps.projects {
		for _, h := range p.allHashes() {
			if other, dup := owner[h]; dup && other != p.Slug {
				return nil, fmt.Errorf("the same API key is configured for projects %q and %q", other, p.Slug)
			}
			owner[h] = p.Slug
		}
	}
	if changed {
		if err := ps.saveLocked(); err != nil {
			return nil, err
		}
	}
	return ps, nil
}

func (p *Project) allHashes() []string {
	hashes := append([]string{}, p.configKeys...)
	for _, k := range p.Keys {
		hashes = append(hashes, k.Hash)
	}
	return hashes
}

// ConfigKeyCount is how many keys come from configuration (shown read-only).
func (p *Project) ConfigKeyCount() int { return len(p.configKeys) }

func (p *Project) clone() *Project {
	data, _ := json.Marshal(p)
	var c Project
	_ = json.Unmarshal(data, &c)
	c.configKeys = append([]string{}, p.configKeys...)
	return &c
}

func (ps *ProjectStore) saveLocked() error {
	list := make([]*Project, 0, len(ps.projects))
	for _, p := range ps.projects {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Slug < list[j].Slug })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := ps.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ps.path)
}

// Authenticate returns the project owning key.
func (ps *ProjectStore) Authenticate(key string) (*Project, bool) {
	if key == "" {
		return nil, false
	}
	h := []byte(hashKey(key))
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	var match *Project
	for _, p := range ps.projects {
		for _, candidate := range p.allHashes() {
			if subtle.ConstantTimeCompare(h, []byte(candidate)) == 1 {
				match = p
			}
		}
	}
	if match == nil {
		return nil, false
	}
	return match.clone(), true
}

func (ps *ProjectStore) Get(slug string) (*Project, bool) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	p, ok := ps.projects[slug]
	if !ok {
		return nil, false
	}
	return p.clone(), true
}

// List returns projects sorted by name.
func (ps *ProjectStore) List() []*Project {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]*Project, 0, len(ps.projects))
	for _, p := range ps.projects {
		out = append(out, p.clone())
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (ps *ProjectStore) Create(name, slug string) (*Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("a project needs a name")
	}
	if slug == "" {
		slug = Slugify(name)
	}
	if !slugPattern.MatchString(slug) {
		return nil, errors.New("the slug must be 1–40 lowercase letters, digits and dashes")
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if _, exists := ps.projects[slug]; exists {
		return nil, ErrProjectExists
	}
	p := &Project{Slug: slug, Name: truncate(name, 80), CreatedAt: time.Now().UTC()}
	ps.projects[slug] = p
	if err := ps.saveLocked(); err != nil {
		delete(ps.projects, slug)
		return nil, err
	}
	return p.clone(), nil
}

// Update applies fn to the stored project and saves it.
func (ps *ProjectStore) Update(slug string, fn func(*Project) error) (*Project, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p, ok := ps.projects[slug]
	if !ok {
		return nil, ErrNotFound
	}
	updated := p.clone()
	if err := fn(updated); err != nil {
		return nil, err
	}
	updated.Slug = slug
	ps.projects[slug] = updated
	if err := ps.saveLocked(); err != nil {
		ps.projects[slug] = p
		return nil, err
	}
	return updated.clone(), nil
}

// AddKey creates a key and returns its plaintext, which is never stored.
func (ps *ProjectStore) AddKey(slug, label string) (string, error) {
	key := NewKey()
	_, err := ps.Update(slug, func(p *Project) error {
		if label = strings.TrimSpace(label); label == "" {
			label = "Key " + time.Now().UTC().Format("2006-01-02")
		}
		p.Keys = append(p.Keys, APIKey{ID: newID(), Label: truncate(label, 60), Prefix: key[:9], Hash: hashKey(key), CreatedAt: time.Now().UTC()})
		return nil
	})
	if err != nil {
		return "", err
	}
	return key, nil
}

func (ps *ProjectStore) RevokeKey(slug, id string) error {
	_, err := ps.Update(slug, func(p *Project) error {
		for i, k := range p.Keys {
			if k.ID == id {
				p.Keys = append(p.Keys[:i], p.Keys[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	})
	return err
}

func (ps *ProjectStore) Delete(slug string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p, ok := ps.projects[slug]
	if !ok {
		return ErrNotFound
	}
	if p.FromConfig && len(p.configKeys) > 0 {
		return errors.New("this project is defined in the server configuration; remove it there first")
	}
	delete(ps.projects, slug)
	return ps.saveLocked()
}

// LoadProjectsFile reads a projects file: a JSON array of ProjectSeed, or {"projects": [...]}.
// ${VAR} references in string values are replaced from the environment.
func LoadProjectsFile(path string, getenv func(string) string) ([]ProjectSeed, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expanded := os.Expand(string(data), func(name string) string {
		v := getenv(name)
		// Keep the result a valid JSON string body.
		enc, _ := json.Marshal(v)
		return strings.Trim(string(enc), `"`)
	})
	var seeds []ProjectSeed
	if err := json.Unmarshal([]byte(expanded), &seeds); err != nil {
		var wrapped struct {
			Projects []ProjectSeed `json:"projects"`
		}
		if err2 := json.Unmarshal([]byte(expanded), &wrapped); err2 != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		seeds = wrapped.Projects
	}
	for i, s := range seeds {
		if s.Slug == "" && s.Name == "" {
			return nil, fmt.Errorf("%s: project %d needs a slug or name", filepath.Base(path), i+1)
		}
		for _, k := range s.Keys {
			if len(k) < 16 {
				return nil, fmt.Errorf("%s: a key for %q is shorter than 16 characters (is its ${VAR} set?)", filepath.Base(path), s.Name+s.Slug)
			}
		}
	}
	return seeds, nil
}
