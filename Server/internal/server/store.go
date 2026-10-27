package server

import (
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

// Manifest mirrors the Swift client's ReportManifest (the `report` multipart part).
type Manifest struct {
	ID          string          `json:"id"`
	CreatedAt   time.Time       `json:"createdAt"`
	Kind        string          `json:"kind"`
	Title       string          `json:"title"`
	Message     string          `json:"message"`
	Contact     string          `json:"contact,omitempty"`
	Tags        []string        `json:"tags"`
	Context     Context         `json:"context"`
	Attachments []ManifestEntry `json:"attachments"`
}

type Context struct {
	App    *AppInfo          `json:"app,omitempty"`
	Device map[string]any    `json:"device,omitempty"`
	Custom map[string]string `json:"custom,omitempty"`
}

type AppInfo struct {
	Name             string `json:"name"`
	BundleIdentifier string `json:"bundleIdentifier,omitempty"`
	Version          string `json:"version,omitempty"`
	Build            string `json:"build,omitempty"`
}

type ManifestEntry struct {
	ID          string `json:"id"`
	Role        string `json:"role"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	Part        string `json:"part"`
}

// Record is a stored report.
type Record struct {
	Manifest
	ReceivedAt time.Time `json:"receivedAt"`
	Project    string    `json:"project"`
	Issue      int       `json:"issue"`
	// App and Status are from before projects and issues existed; read only for migration.
	App        string                   `json:"app,omitempty"`
	Status     string                   `json:"status,omitempty"`
	Files      []StoredFile             `json:"files"`
	Forwarding map[string]*ForwardState `json:"forwarding,omitempty"`
	RemoteAddr string                   `json:"-"`
	UserAgent  string                   `json:"userAgent,omitempty"`
	// ReaderTokenHash lets the reporter's app read this report's status and thread.
	ReaderTokenHash string `json:"readerTokenHash,omitempty"`
}

type StoredFile struct {
	Name        string `json:"name"` // on disk, unique within the report
	Filename    string `json:"filename"`
	Role        string `json:"role"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

type ForwardState struct {
	Delivered   bool      `json:"delivered"`
	URL         string    `json:"url,omitempty"`
	Error       string    `json:"error,omitempty"`
	Attempts    int       `json:"attempts"`
	NextAttempt time.Time `json:"nextAttempt,omitempty"`
	GaveUp      bool      `json:"gaveUp,omitempty"`
}

func (f StoredFile) IsImage() bool { return strings.HasPrefix(f.ContentType, "image/") }
func (f StoredFile) IsVideo() bool { return f.ContentType == "video/mp4" }
func (f StoredFile) IsText() bool {
	return strings.HasPrefix(f.ContentType, "text/") || strings.HasPrefix(f.ContentType, "application/json")
}

var uuidPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// ValidID reports whether id is a UUID, which is also what keeps it safe as a directory name.
func ValidID(id string) bool { return uuidPattern.MatchString(id) }

// Store keeps reports as plain files: <dir>/reports/<id>/report.json plus one file per attachment.
// Everything is also indexed in memory; it's meant for thousands of reports, not millions.
type Store struct {
	dir     string
	mu      sync.RWMutex
	records map[string]*Record
}

var ErrNotFound = errors.New("report not found")

func OpenStore(dir string) (*Store, error) {
	s := &Store{dir: dir, records: map[string]*Record{}}
	root := filepath.Join(dir, "reports")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !ValidID(entry.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name(), "report.json"))
		if err != nil {
			// Left behind by a crash mid-upload; the client will retry.
			_ = os.RemoveAll(filepath.Join(root, entry.Name()))
			continue
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil || rec.ID != entry.Name() {
			continue
		}
		s.records[rec.ID] = &rec
	}
	return s, nil
}

func (s *Store) reportDir(id string) string {
	return filepath.Join(s.dir, "reports", strings.ToLower(id))
}

// Stage returns a fresh directory for an incoming report's files. Commit or Discard it.
func (s *Store) Stage() (string, error) {
	tmp := filepath.Join(s.dir, "tmp")
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return "", err
	}
	return os.MkdirTemp(tmp, "upload-")
}

// Commit moves a staged upload into place. If the report already exists the staged files are
// discarded and the existing record is returned with created=false.
func (s *Store) Commit(staged string, rec *Record) (*Record, bool, error) {
	rec.ID = strings.ToLower(rec.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.records[rec.ID]; ok {
		_ = os.RemoveAll(staged)
		return existing.clone(), false, nil
	}
	if err := writeJSON(filepath.Join(staged, "report.json"), rec); err != nil {
		_ = os.RemoveAll(staged)
		return nil, false, err
	}
	if err := os.Rename(staged, s.reportDir(rec.ID)); err != nil {
		_ = os.RemoveAll(staged)
		return nil, false, err
	}
	s.records[rec.ID] = rec.clone()
	return rec.clone(), true, nil
}

func (s *Store) Get(id string) (*Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[strings.ToLower(id)]
	if !ok {
		return nil, ErrNotFound
	}
	return rec.clone(), nil
}

// Update applies fn to a copy of the record and persists it.
func (s *Store) Update(id string, fn func(*Record)) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[strings.ToLower(id)]
	if !ok {
		return nil, ErrNotFound
	}
	updated := rec.clone()
	fn(updated)
	if err := writeJSON(filepath.Join(s.reportDir(updated.ID), "report.json"), updated); err != nil {
		return nil, err
	}
	s.records[updated.ID] = updated
	return updated.clone(), nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id = strings.ToLower(id)
	if _, ok := s.records[id]; !ok {
		return ErrNotFound
	}
	delete(s.records, id)
	return os.RemoveAll(s.reportDir(id))
}

// FilePath returns the on-disk path of an attachment, or ErrNotFound.
func (s *Store) FilePath(id, name string) (string, StoredFile, error) {
	rec, err := s.Get(id)
	if err != nil {
		return "", StoredFile{}, err
	}
	for _, f := range rec.Files {
		if f.Name == name {
			return filepath.Join(s.reportDir(rec.ID), f.Name), f, nil
		}
	}
	return "", StoredFile{}, ErrNotFound
}

type Filter struct {
	Project string
	Kind    string
	Query   string
	Limit   int
	Offset  int
}

// List returns matching reports, newest first, and the total number of matches.
func (s *Store) List(f Filter) ([]*Record, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q := strings.ToLower(strings.TrimSpace(f.Query))
	var out []*Record
	for _, rec := range s.records {
		if f.Project != "" && rec.Project != f.Project {
			continue
		}
		if f.Kind != "" && rec.Kind != f.Kind {
			continue
		}
		if q != "" && !rec.matches(q) {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceivedAt.After(out[j].ReceivedAt) })
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
	clones := make([]*Record, len(out))
	for i, rec := range out {
		clones[i] = rec.clone()
	}
	return clones, total
}

// Pending returns reports with forwards still to deliver.
func (s *Store) Pending() []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Record
	for _, rec := range s.records {
		for _, st := range rec.Forwarding {
			if !st.Delivered && !st.GaveUp {
				out = append(out, rec.clone())
				break
			}
		}
	}
	return out
}

// Prune deletes reports received before cutoff and returns how many it removed.
func (s *Store) Prune(cutoff time.Time) int {
	s.mu.RLock()
	var old []string
	for id, rec := range s.records {
		if rec.ReceivedAt.Before(cutoff) {
			old = append(old, id)
		}
	}
	s.mu.RUnlock()
	for _, id := range old {
		_ = s.Delete(id)
	}
	return len(old)
}

func (r *Record) matches(q string) bool {
	fields := []string{r.ID, r.Title, r.Message, r.Contact, r.Kind, strings.Join(r.Tags, " ")}
	if r.Context.App != nil {
		fields = append(fields, r.Context.App.Name, r.Context.App.Version, r.Context.App.Build)
	}
	for _, v := range r.Context.Custom {
		fields = append(fields, v)
	}
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}
	return false
}

func (r *Record) clone() *Record {
	data, _ := json.Marshal(r)
	var c Record
	_ = json.Unmarshal(data, &c)
	c.RemoteAddr = r.RemoteAddr
	return &c
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving %s: %w", filepath.Base(path), err)
	}
	return nil
}

// ForIssue returns an issue's reports in the order given.
func (s *Store) ForIssue(ids []string) []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Record, 0, len(ids))
	for _, id := range ids {
		if rec, ok := s.records[strings.ToLower(id)]; ok {
			out = append(out, rec.clone())
		}
	}
	return out
}

// All returns every record (for migration).
func (s *Store) All() []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Record, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, rec.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceivedAt.Before(out[j].ReceivedAt) })
	return out
}

// Matches reports whether any of ids matches a lowercase query (used by issue search).
func (s *Store) Matches(ids []string, q string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range ids {
		if rec, ok := s.records[strings.ToLower(id)]; ok && rec.matches(q) {
			return true
		}
	}
	return false
}
