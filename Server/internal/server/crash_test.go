package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// testdata/Demo.dwarf is the dSYM of testdata/Demo.swift, built with
// swiftc -target arm64-apple-macos14 -g -Onone and dsymutil.
const demoUUID = "A078804C-AD4D-3360-BBF9-75FEA5D505D9"

func crashUpload(id, title string) upload {
	diag := map[string]any{
		"version": "1.0.0",
		"callStackTree": map[string]any{"callStackPerThread": true, "callStacks": []any{
			map[string]any{"threadAttributed": true, "callStackRootFrames": []any{
				map[string]any{"binaryUUID": "9A0E4C3F-1B6B-3D6E-8A7B-2E3C4D5E6F70", "binaryName": "libswiftCore.dylib", "offsetIntoBinaryTextSegment": 123456, "address": 7000000000,
					"subFrames": []any{map[string]any{"binaryUUID": demoUUID, "binaryName": "Demo", "offsetIntoBinaryTextSegment": 0x8bc, "address": 4295000000,
						"subFrames": []any{map[string]any{"binaryUUID": demoUUID, "binaryName": "Demo", "offsetIntoBinaryTextSegment": 2672.0}}}}},
			}},
			map[string]any{"threadAttributed": false, "callStackRootFrames": []any{
				map[string]any{"binaryUUID": "11111111-2222-3333-4444-555555555555", "binaryName": "libsystem_kernel.dylib", "offsetIntoBinaryTextSegment": "777"},
			}},
		}},
		"diagnosticMetaData": map[string]any{"appVersion": "1.2.0", "appBuildVersion": "42"},
	}
	data, _ := json.Marshal(diag)
	crashLog := []byte("Crash log from the device")
	return upload{
		manifest: map[string]any{
			"id": id, "createdAt": "2026-10-27T10:00:00Z", "kind": "crash",
			"title": title, "message": "EXC_BREAKPOINT\nTop app frame: Demo +0x8bc\nVersion: 1.2.0 (42)", "tags": []string{"metrickit"},
			"context": map[string]any{"app": map[string]any{"name": "Demo", "version": "1.2.0", "build": "42"}},
			"attachments": []map[string]any{
				{"id": "B0000000-0000-0000-0000-000000000000", "role": "log", "filename": "crash.txt", "contentType": "text/plain", "size": len(crashLog), "part": "attachment0"},
				{"id": "B1000000-0000-0000-0000-000000000000", "role": "file", "filename": "diagnostic.json", "contentType": "application/json", "size": len(data), "part": "attachment1"},
			},
		},
		files: map[string][]byte{"attachment0": crashLog, "attachment1": data},
		types: map[string]string{"attachment0": "text/plain", "attachment1": "application/json"},
	}
}

func uploadSymbols(srv *Server, token string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/symbols", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func TestSymbolFileLookup(t *testing.T) {
	data, err := os.ReadFile("testdata/Demo.dwarf")
	if err != nil {
		t.Fatal(err)
	}
	sf, err := parseSymbolFile(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		offset uint64
		fn     string
		line   int
	}{{0x8bc, "ContentView.load", 4}, {0xa70, "Network.fetch", 17}} {
		got, ok := sf.lookup(sf.textAddr + c.offset - 1)
		if !ok || got.Function != c.fn || got.File != "Demo.swift" || got.Line != c.line {
			t.Errorf("0x%x: got %+v, want %s line %d", c.offset, got, c.fn, c.line)
		}
	}
	if _, ok := sf.lookup(sf.textAddr + 0x10); ok {
		t.Error("an address before any function shouldn't resolve")
	}
}

func TestCrashSymbolication(t *testing.T) {
	cfg := testConfig(t)
	cfg.Seeds[0].GroupDuplicates = boolPtr(true)
	srv, store := newTestServer(t, cfg)
	token, err := srv.Projects().AddScopedKey("gelo", "CI", KeyScopeSymbols)
	if err != nil {
		t.Fatal(err)
	}
	dwarfFile, _ := os.ReadFile("testdata/Demo.dwarf")

	// The ingest key can't upload symbols, and a symbol token can't send reports.
	if rr := uploadSymbols(srv, testKey, dwarfFile); rr.Code != http.StatusForbidden {
		t.Fatalf("ingest key: %d", rr.Code)
	}
	if rr := post(t, srv, sampleUpload(id1), token); rr.Code != http.StatusUnauthorized {
		t.Fatalf("symbol token sent a report: %d", rr.Code)
	}

	// A crash before the symbols arrive is stored as is, noting what's missing.
	if rr := post(t, srv, crashUpload(id1, "Crash: EXC_BREAKPOINT in Demo +0x8bc"), testKey); rr.Code != http.StatusCreated {
		t.Fatalf("crash: %d %s", rr.Code, rr.Body)
	}
	first, _ := store.Get(strings.ToLower(id1))
	if first.Symbolication == nil || first.Symbolication.Resolved != 0 || len(first.Symbolication.Missing) != 1 || first.Symbolication.Missing[0].UUID != demoUUID {
		t.Fatalf("symbolication before upload: %+v", first.Symbolication)
	}

	// Uploading a zipped dSYM symbolicates it after the fact.
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	fw, _ := zw.Create("Demo.app.dSYM/Contents/Resources/DWARF/Demo")
	_, _ = fw.Write(dwarfFile)
	_, _ = zw.Create("Demo.app.dSYM/Contents/Info.plist")
	_ = zw.Close()
	rr := uploadSymbols(srv, token, zipped.Bytes())
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body)
	}
	var resp struct {
		Symbols        []SymbolMeta `json:"symbols"`
		Resymbolicated int          `json:"resymbolicated"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Symbols) != 1 || resp.Symbols[0].UUID != demoUUID || resp.Symbols[0].Arch != "arm64" || resp.Resymbolicated != 1 {
		t.Fatalf("upload response: %s", rr.Body)
	}
	first, _ = store.Get(strings.ToLower(id1))
	if first.Symbolication.Resolved != 2 || len(first.Symbolication.Missing) != 0 || first.Files[0].Filename != symbolicatedFilename {
		t.Fatalf("after upload: %+v files %+v", first.Symbolication, first.Files)
	}
	text := readStored(t, store, first.ID, first.Files[0].Name)
	for _, want := range []string{"2 of 4 frames resolved", "1   Demo                         ContentView.load (Demo.swift:4)", "Network.fetch (Demo.swift:17)", "libswiftCore.dylib           +0x1e240"} {
		if !strings.Contains(text, want) {
			t.Errorf("symbolicated log lacks %q:\n%s", want, text)
		}
	}

	// With symbols present, a new crash gets the function in its title and groups by it.
	rr = post(t, srv, crashUpload(id2, "Crash: EXC_BREAKPOINT in Demo +0x8bc"), testKey)
	if rr.Code != http.StatusCreated {
		t.Fatalf("second crash: %d", rr.Code)
	}
	second, _ := store.Get(strings.ToLower(id2))
	if second.Title != "Crash: EXC_BREAKPOINT in ContentView.load" || !strings.Contains(second.Message, "Top app frame: ContentView.load\n") {
		t.Fatalf("title %q message %q", second.Title, second.Message)
	}
	page := adminGet(srv, "/p/gelo/issues/"+itoa(second.Issue)).Body.String()
	if !strings.Contains(page, "symbolicated") || !strings.Contains(page, "ContentView.load (Demo.swift:4)") {
		t.Error("issue page doesn't show the symbolicated log")
	}
	settings := adminGet(srv, "/p/gelo/settings").Body.String()
	if !strings.Contains(settings, demoUUID) {
		t.Error("settings don't list the symbols")
	}

	// Garbage is refused.
	if rr := uploadSymbols(srv, token, []byte("not a dsym")); rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("garbage: %d", rr.Code)
	}
}

func readStored(t *testing.T, store *Store, id, name string) string {
	t.Helper()
	path, _, err := store.FilePath(id, name)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func boolPtr(b bool) *bool { return &b }

func itoa(n int) string { return strconv.Itoa(n) }
