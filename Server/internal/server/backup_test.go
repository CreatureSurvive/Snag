package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const id2 = "7D2B9E61-1A44-4F0B-9C3E-0B8A2F4C5D6E"

func TestBackupRestoreRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	srv, _ := newTestServer(t, cfg)
	if rr := post(t, srv, sampleUpload(id1), testKey); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body)
	}
	// A leftover temp file and an upload in progress stay out of the backup.
	_ = os.WriteFile(filepath.Join(cfg.DataDir, "projects.json.tmp"), []byte("x"), 0o640)
	_ = os.MkdirAll(filepath.Join(cfg.DataDir, "tmp", "upload-1"), 0o750)
	var buf bytes.Buffer
	if err := WriteBackup(cfg.DataDir, &buf); err != nil {
		t.Fatal(err)
	}
	names := archiveNames(t, buf.Bytes())
	for _, n := range names {
		if strings.HasPrefix(n, "tmp/") || strings.HasSuffix(n, ".tmp") {
			t.Errorf("backup includes %s", n)
		}
	}

	target := testConfig(t)
	target.Seeds = nil
	summary, err := Restore(target, bytes.NewReader(buf.Bytes()), RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary != "1 project(s), 1 issue(s), 1 report(s)" {
		t.Errorf("summary %q", summary)
	}
	restored, store := newTestServer(t, target)
	rec, err := store.Get(strings.ToLower(id1))
	if err != nil || rec.Issue != 1 {
		t.Fatalf("restored report: %v %+v", err, rec)
	}
	if p, ok := restored.Projects().Authenticate(testKey); ok {
		t.Errorf("config keys aren't stored, so they shouldn't come back: %s", p.Slug)
	}
	if rr := adminGet(restored, "/reports/"+rec.ID+"/files/"+rec.Files[0].Name); rr.Code != http.StatusOK {
		t.Errorf("attachment after restore: %d", rr.Code)
	}

	// Restoring over data is refused, unless the current data is set aside.
	if _, err := Restore(target, bytes.NewReader(buf.Bytes()), RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "isn't empty") {
		t.Fatalf("expected refusal, got %v", err)
	}
	summary, err = Restore(target, bytes.NewReader(buf.Bytes()), RestoreOptions{Replace: true})
	if err != nil || !strings.Contains(summary, "replaced-") {
		t.Fatalf("replace: %v %q", err, summary)
	}
}

func TestProjectExportMergesIntoAnotherServer(t *testing.T) {
	cfg := testConfig(t)
	srv, _ := newTestServer(t, cfg)
	post(t, srv, sampleUpload(id1), testKey)
	rr := adminPost(t, srv, "/p/gelo/export", nil)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("export: %d %s", rr.Code, rr.Header())
	}
	export := rr.Body.Bytes()

	other := testConfig(t)
	other.Seeds = []ProjectSeed{{Slug: "rolly", Name: "Rolly", Keys: []string{"another-key-0123456789"}}}
	osrv, _ := newTestServer(t, other)
	u := sampleUpload(id2)
	post(t, osrv, u, "another-key-0123456789")
	other.Seeds = nil
	if _, err := Restore(other, bytes.NewReader(export), RestoreOptions{Merge: true}); err != nil {
		t.Fatal(err)
	}
	merged, store := newTestServer(t, other)
	if _, ok := merged.Projects().Get("gelo"); !ok {
		t.Fatal("gelo wasn't merged")
	}
	if _, ok := merged.Projects().Get("rolly"); !ok {
		t.Fatal("rolly was lost")
	}
	if len(store.All()) != 2 || merged.Issues().Count() != 2 {
		t.Fatalf("reports %d, issues %d", len(store.All()), merged.Issues().Count())
	}
	if _, err := Restore(other, bytes.NewReader(export), RestoreOptions{Merge: true}); err == nil {
		t.Fatal("merging the same project twice should fail")
	}
}

func TestRestoreRejectsUnsafeArchives(t *testing.T) {
	for name, entry := range map[string]string{
		"traversal": "../evil.json",
		"absolute":  "/etc/passwd",
		"foreign":   "bin/sh",
		"bad id":    "reports/not-a-uuid/report.json",
	} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(&tar.Header{Name: entry, Mode: 0o640, Size: 2})
		_, _ = tw.Write([]byte("{}"))
		_ = tw.Close()
		_ = gz.Close()
		cfg := testConfig(t)
		if _, err := Restore(cfg, &buf, RestoreOptions{}); err == nil {
			t.Errorf("%s: accepted %q", name, entry)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.DataDir), "evil.json")); err == nil {
			t.Errorf("%s: wrote outside the data directory", name)
		}
	}
}

func TestDataDirLock(t *testing.T) {
	dir := t.TempDir()
	release, err := LockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockDataDir(dir); err == nil {
		t.Fatal("a second lock should fail")
	}
	release()
	again, err := LockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestScheduledBackupKeepsNewest(t *testing.T) {
	cfg := testConfig(t)
	cfg.BackupDir = t.TempDir()
	cfg.BackupKeep = 2
	newTestServer(t, cfg)
	for _, stamp := range []string{"20200101-000000", "20200102-000000", "20200103-000000"} {
		_ = os.WriteFile(filepath.Join(cfg.BackupDir, "snag-backup-"+stamp+".tar.gz"), nil, 0o640)
	}
	path, err := RunScheduledBackup(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(cfg.BackupDir)
	if len(entries) != 2 || entries[1].Name() != filepath.Base(path) {
		t.Fatalf("kept %v", entries)
	}
}

func archiveNames(t *testing.T, data []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	return names
}
