package server

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backups are gzipped tarballs of the data directory. Every file the server writes is replaced
// atomically, so a backup taken while the server runs holds whole files; at worst a report
// received mid-backup is missing from it.

const maxRestoreFileBytes = 1 << 30

// skipInBackup leaves out uploads in progress, temporary files and earlier restores' leftovers.
func skipInBackup(rel string) bool {
	first, _, _ := strings.Cut(rel, "/")
	return first == "tmp" || strings.HasPrefix(first, "replaced-") || first == ".snag.lock" ||
		strings.HasSuffix(rel, ".tmp")
}

// WriteBackup writes the whole data directory to w.
func WriteBackup(dataDir string, w io.Writer) error {
	return writeArchive(w, func(add func(rel string) error) error {
		return filepath.WalkDir(dataDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dataDir, p)
			rel = filepath.ToSlash(rel)
			if rel == "." {
				return nil
			}
			if skipInBackup(rel) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() {
				return add(rel)
			}
			return nil
		})
	}, dataDir)
}

func writeArchive(w io.Writer, walk func(add func(rel string) error) error, dataDir string, extra ...archiveFile) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, f := range extra {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o640, Size: int64(len(f.data)), ModTime: time.Now()}); err != nil {
			return err
		}
		if _, err := tw.Write(f.data); err != nil {
			return err
		}
	}
	err := walk(func(rel string) error {
		f, err := os.Open(filepath.Join(dataDir, filepath.FromSlash(rel)))
		if errors.Is(err, os.ErrNotExist) {
			return nil // deleted while walking
		}
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o640, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
			return err
		}
		_, err = io.CopyN(tw, f, info.Size())
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

type archiveFile struct {
	name string
	data []byte
}

// WriteProjectExport writes one project, its issues and its reports in the data directory
// layout, so it can be restored into an empty server or merged into another one.
func (s *Server) WriteProjectExport(slug string, w io.Writer) error {
	p, ok := s.projects.Get(slug)
	if !ok {
		return errors.New("no such project")
	}
	projects, err := json.MarshalIndent([]*Project{p}, "", "  ")
	if err != nil {
		return err
	}
	var reports []string
	for _, rec := range s.store.All() {
		if rec.Project == slug {
			reports = append(reports, rec.ID)
		}
	}
	dir := s.cfg.DataDir
	return writeArchive(w, func(add func(string) error) error {
		files, _ := os.ReadDir(filepath.Join(dir, "issues", slug))
		for _, f := range files {
			if f.Type().IsRegular() && !strings.HasSuffix(f.Name(), ".tmp") {
				if err := add("issues/" + slug + "/" + f.Name()); err != nil {
					return err
				}
			}
		}
		syms, _ := os.ReadDir(filepath.Join(dir, "symbols", slug))
		for _, u := range syms {
			for _, name := range []string{"meta.json", "macho"} {
				if _, err := os.Stat(filepath.Join(dir, "symbols", slug, u.Name(), name)); err == nil {
					if err := add("symbols/" + slug + "/" + u.Name() + "/" + name); err != nil {
						return err
					}
				}
			}
		}
		for _, id := range reports {
			files, _ := os.ReadDir(filepath.Join(dir, "reports", id))
			for _, f := range files {
				if f.Type().IsRegular() && !strings.HasSuffix(f.Name(), ".tmp") {
					if err := add("reports/" + id + "/" + f.Name()); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}, dir, archiveFile{"projects.json", projects})
}

// extractArchive unpacks a backup into dir, accepting only regular files at clean relative
// paths inside the data layout.
func extractArchive(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("not a gzipped backup: %w", err)
	}
	tr := tar.NewReader(gz)
	count := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading backup: %w", err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return fmt.Errorf("backup entry %q isn't a regular file", h.Name)
		}
		name := path.Clean(h.Name)
		if name != h.Name || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
			return fmt.Errorf("backup entry %q has an unsafe path", h.Name)
		}
		if !allowedInBackup(name) {
			return fmt.Errorf("backup entry %q isn't part of a Snag data directory", h.Name)
		}
		if h.Size > maxRestoreFileBytes {
			return fmt.Errorf("backup entry %q is too large", h.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			return err
		}
		_, err = io.CopyN(f, tr, h.Size)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		return errors.New("the backup is empty")
	}
	return nil
}

func allowedInBackup(name string) bool {
	parts := strings.Split(name, "/")
	switch {
	case len(parts) == 1:
		return strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".key")
	case parts[0] == "reports" && len(parts) == 3:
		return ValidID(parts[1])
	case parts[0] == "issues" && len(parts) == 3:
		return slugPattern.MatchString(parts[1])
	case parts[0] == "symbols" && len(parts) == 4:
		return slugPattern.MatchString(parts[1]) && ValidID(parts[2]) && (parts[3] == "macho" || parts[3] == "meta.json")
	}
	return false
}

// RestoreOptions control Restore.
type RestoreOptions struct {
	// Merge adds the backup's projects to the existing data; projects that already exist are
	// refused. Without it, the data directory must be empty unless Replace is set.
	Merge bool
	// Replace moves the current data into <data>/replaced-<time>/ first.
	Replace bool
}

// Restore loads a backup or project export into cfg.DataDir. The server must not be running
// on that directory (LockDataDir enforces it). The backup is unpacked and opened in a scratch
// directory first, so a corrupt one changes nothing.
func Restore(cfg Config, r io.Reader, opts RestoreOptions) (summary string, err error) {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return "", err
	}
	scratch, err := os.MkdirTemp(cfg.DataDir, "tmp-restore-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch)
	if err := extractArchive(r, scratch); err != nil {
		return "", err
	}
	check := cfg
	check.DataDir = scratch
	check.Seeds = nil
	incoming, err := Open(check, nil)
	if err != nil {
		return "", fmt.Errorf("the backup doesn't open: %w", err)
	}
	summary = fmt.Sprintf("%d project(s), %d issue(s), %d report(s)", len(incoming.projects.List()), incoming.issues.Count(), len(incoming.store.All()))

	entries, err := os.ReadDir(cfg.DataDir)
	if err != nil {
		return "", err
	}
	var existing []string
	for _, e := range entries {
		if n := e.Name(); n != filepath.Base(scratch) && n != ".snag.lock" && n != "tmp" && !strings.HasPrefix(n, "replaced-") {
			existing = append(existing, n)
		}
	}
	if opts.Merge {
		return summary, mergeInto(cfg, scratch, incoming)
	}
	if len(existing) > 0 {
		if !opts.Replace {
			return "", errors.New("the data directory isn't empty; use --merge to add projects or --replace to set the current data aside")
		}
		aside := filepath.Join(cfg.DataDir, "replaced-"+time.Now().UTC().Format("20060102-150405"))
		if err := os.Mkdir(aside, 0o750); err != nil {
			return "", err
		}
		for _, n := range existing {
			if err := os.Rename(filepath.Join(cfg.DataDir, n), filepath.Join(aside, n)); err != nil {
				return "", err
			}
		}
		summary += "; the previous data is in " + filepath.Base(aside)
	}
	moved, _ := os.ReadDir(scratch)
	for _, e := range moved {
		if err := os.Rename(filepath.Join(scratch, e.Name()), filepath.Join(cfg.DataDir, e.Name())); err != nil {
			return "", err
		}
	}
	return summary, nil
}

// mergeInto copies the backup's projects, issues and reports into the live data directory.
func mergeInto(cfg Config, scratch string, incoming *Server) error {
	current := cfg
	current.Seeds = nil
	live, err := Open(current, nil)
	if err != nil {
		return err
	}
	var add []*Project
	for _, p := range incoming.projects.List() {
		if _, exists := live.projects.Get(p.Slug); exists {
			return fmt.Errorf("project %q already exists here", p.Slug)
		}
		p.FromConfig = false
		add = append(add, p)
	}
	for _, rec := range incoming.store.All() {
		if _, err := live.store.Get(rec.ID); err == nil {
			return fmt.Errorf("report %s already exists here", rec.ID)
		}
	}
	for _, p := range add {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, "issues"), 0o750); err != nil {
			return err
		}
		src := filepath.Join(scratch, "issues", p.Slug)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, filepath.Join(cfg.DataDir, "issues", p.Slug)); err != nil {
				return err
			}
		}
		if src := filepath.Join(scratch, "symbols", p.Slug); dirExists(src) {
			if err := os.MkdirAll(filepath.Join(cfg.DataDir, "symbols"), 0o750); err != nil {
				return err
			}
			if err := os.Rename(src, filepath.Join(cfg.DataDir, "symbols", p.Slug)); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "reports"), 0o750); err != nil {
		return err
	}
	for _, rec := range incoming.store.All() {
		if err := os.Rename(filepath.Join(scratch, "reports", rec.ID), filepath.Join(cfg.DataDir, "reports", rec.ID)); err != nil {
			return err
		}
	}
	return live.projects.addRestored(add)
}

// RunScheduledBackup writes a timestamped backup into cfg.BackupDir and keeps the newest
// cfg.BackupKeep.
func RunScheduledBackup(cfg Config) (string, error) {
	if err := os.MkdirAll(cfg.BackupDir, 0o750); err != nil {
		return "", err
	}
	name := "snag-backup-" + time.Now().UTC().Format("20060102-150405") + ".tar.gz"
	final := filepath.Join(cfg.BackupDir, name)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return "", err
	}
	err = WriteBackup(cfg.DataDir, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if cfg.BackupKeep > 0 {
		entries, _ := os.ReadDir(cfg.BackupDir)
		var names []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "snag-backup-") && strings.HasSuffix(e.Name(), ".tar.gz") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for len(names) > cfg.BackupKeep {
			_ = os.Remove(filepath.Join(cfg.BackupDir, names[0]))
			names = names[1:]
		}
	}
	return final, nil
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="snag-backup-`+time.Now().UTC().Format("20060102-150405")+`.tar.gz"`)
	if err := WriteBackup(s.cfg.DataDir, w); err != nil {
		s.log.Error("backup", "err", err)
	}
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="snag-`+p.Slug+`-`+time.Now().UTC().Format("20060102")+`.tar.gz"`)
	if err := s.WriteProjectExport(p.Slug, w); err != nil {
		s.log.Error("project export", "project", p.Slug, "err", err)
	}
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
