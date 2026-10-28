package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxSymbolsBytes = 1 << 30

// handleSymbolsUpload takes a dSYM (a zip of .dSYM bundles, or a DWARF Mach-O file) as the raw
// request body, authenticated with a project's symbol upload token.
//
//	curl --data-binary @MyApp.app.dSYM.zip -H "Authorization: Bearer $SNAG_SYMBOLS_TOKEN" \
//	  https://snag.example.com/api/v1/symbols
func (s *Server) handleSymbolsUpload(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	project, ok := s.projects.AuthenticateSymbols(strings.TrimSpace(token))
	if !ok {
		if _, ingest := s.projects.Authenticate(strings.TrimSpace(token)); ingest {
			fail(w, http.StatusForbidden, "that's an ingest key; create a symbol upload token in the project's settings")
			return
		}
		fail(w, http.StatusUnauthorized, "missing or invalid symbol upload token")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSymbolsBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, http.StatusRequestEntityTooLarge, "symbols are limited to 1 GB per upload")
		} else {
			fail(w, http.StatusBadRequest, "reading upload: %v", err)
		}
		return
	}
	added, n, err := s.addSymbols(project.Slug, data)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	writeJSONResponse(w, http.StatusCreated, map[string]any{"symbols": added, "resymbolicated": n})
}

func (s *Server) addSymbols(project string, data []byte) ([]SymbolMeta, int, error) {
	added, err := s.symbols.Add(project, data)
	if err != nil {
		return nil, 0, err
	}
	uuids := make([]string, len(added))
	for i, m := range added {
		uuids[i] = m.UUID
	}
	n := s.resymbolicate(project, uuids)
	s.log.Info("symbols uploaded", "project", project, "count", len(added), "resymbolicated", n)
	return added, n, nil
}

func (s *Server) handleSymbolsForm(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	back := func(msg string) {
		http.Redirect(w, r, "/p/"+p.Slug+"/settings?flash="+url.QueryEscape(msg)+"#symbols", http.StatusSeeOther)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSymbolsBytes)
	file, _, err := r.FormFile("file")
	if err != nil {
		back("Choose a zipped .dSYM to upload")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		back("Upload failed: " + err.Error())
		return
	}
	added, n, err := s.addSymbols(p.Slug, data)
	if err != nil {
		back("Upload failed: " + err.Error())
		return
	}
	msg := fmt.Sprintf("Added symbols for %d binar%s", len(added), map[bool]string{true: "y", false: "ies"}[len(added) == 1])
	if n > 0 {
		msg += fmt.Sprintf("; symbolicated %d earlier report%s", n, map[bool]string{true: "", false: "s"}[n == 1])
	}
	back(msg)
}

func (s *Server) handleDeleteSymbols(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	_ = s.symbols.Delete(p.Slug, r.PathValue("uuid"))
	http.Redirect(w, r, "/p/"+p.Slug+"/settings?flash=Symbols+deleted#symbols", http.StatusSeeOther)
}

func (s *Server) handleCreateSymbolKey(w http.ResponseWriter, r *http.Request) {
	p := projectFrom(r)
	label := r.FormValue("label")
	if strings.TrimSpace(label) == "" {
		label = "Symbols"
	}
	key, err := s.projects.AddScopedKey(p.Slug, label, KeyScopeSymbols)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p, _ = s.projects.Get(p.Slug)
	s.renderSettingsKey(w, p, key, KeyScopeSymbols)
}
