package server

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
)

// The browser SDK is served from the server itself: <script src="https://snag.example.com/sdk/snag.js">.

//go:embed sdk/snag.js
var webSDK []byte

var webSDKTag = func() string {
	sum := sha256.Sum256(webSDK)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}()

func (s *Server) handleWebSDK(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("ETag", webSDKTag)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	if r.Header.Get("If-None-Match") == webSDKTag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(webSDK)
}

// normalizeOrigin returns scheme://host[:port] for an origin or URL, or "".
func normalizeOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// originAllowed reports whether a project accepts browser uploads from origin.
func (p *Project) originAllowed(origin string) bool {
	origin = normalizeOrigin(origin)
	for _, o := range p.WebOrigins {
		if o == "*" || normalizeOrigin(o) == origin {
			return origin != ""
		}
	}
	return false
}

// handleReportsPreflight answers CORS preflights for uploads from web pages. The preflight
// carries no key, so it's allowed when any project lists the origin; the upload itself is then
// checked against its own project's origins.
func (s *Server) handleReportsPreflight(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	allowed := false
	for _, p := range s.projects.List() {
		allowed = allowed || p.originAllowed(origin)
	}
	w.Header().Add("Vary", "Origin")
	if !allowed {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	setCORS(w, origin)
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func setCORS(w http.ResponseWriter, origin string) {
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Add("Vary", "Origin")
}

// checkOrigin runs for uploads: requests from a browser (with an Origin header) must come from
// one of the project's web origins. Native apps send no Origin.
func (s *Server) checkOrigin(w http.ResponseWriter, r *http.Request, p *Project) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if !p.originAllowed(origin) {
		fail(w, http.StatusForbidden, "this project doesn't accept reports from %s; add it under Web origins in the project's settings", origin)
		return false
	}
	setCORS(w, origin)
	return true
}
