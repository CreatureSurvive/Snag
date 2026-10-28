package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Dashboard sign-in: a login page (password or passkey) and session cookies. HTTP Basic auth
// still works for scripts and the JSON API.

const (
	sessionCookie   = "snag_session"
	sessionLifetime = 30 * 24 * time.Hour
)

// Passkey is a WebAuthn credential (ES256) registered for the admin.
type Passkey struct {
	ID         string    `json:"id"` // base64url credential ID
	Name       string    `json:"name"`
	X          []byte    `json:"x"`
	Y          []byte    `json:"y"`
	SignCount  uint32    `json:"signCount"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
}

// Auth keeps sessions (hashed) and passkeys in <data>/auth.json.
type Auth struct {
	path string
	mu   sync.Mutex
	data authFile
	// challenges are single-use WebAuthn challenges, by value.
	challenges map[string]authChallenge
	now        func() time.Time
}

type authFile struct {
	Sessions map[string]time.Time `json:"sessions"` // sha256(token) -> expiry
	Passkeys []Passkey            `json:"passkeys"`
	UserID   []byte               `json:"userId"` // WebAuthn user handle
}

type authChallenge struct {
	purpose string // "login" or "register"
	expires time.Time
}

func OpenAuth(dataDir string) (*Auth, error) {
	a := &Auth{path: filepath.Join(dataDir, "auth.json"), challenges: map[string]authChallenge{}, now: time.Now}
	if data, err := os.ReadFile(a.path); err == nil {
		if err := json.Unmarshal(data, &a.data); err != nil {
			return nil, fmt.Errorf("auth.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if a.data.Sessions == nil {
		a.data.Sessions = map[string]time.Time{}
	}
	if len(a.data.UserID) == 0 {
		a.data.UserID = randomBytes(16)
	}
	return a, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func (a *Auth) saveLocked() error {
	now := a.now()
	for k, exp := range a.data.Sessions {
		if exp.Before(now) {
			delete(a.data.Sessions, k)
		}
	}
	return writeJSONMode(a.path, a.data, 0o600)
}

func writeJSONMode(path string, v any, mode os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", data, mode); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (a *Auth) NewSession() (string, error) {
	token := hex.EncodeToString(randomBytes(32))
	a.mu.Lock()
	defer a.mu.Unlock()
	a.data.Sessions[hashKey(token)] = a.now().Add(sessionLifetime)
	return token, a.saveLocked()
}

// Valid checks a session token and extends it once a day.
func (a *Auth) Valid(token string) bool {
	if token == "" {
		return false
	}
	h := hashKey(token)
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.data.Sessions[h]
	if !ok || exp.Before(a.now()) {
		return false
	}
	if renewed := a.now().Add(sessionLifetime); renewed.Sub(exp) > 24*time.Hour {
		a.data.Sessions[h] = renewed
		_ = a.saveLocked()
	}
	return true
}

func (a *Auth) EndSession(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.data.Sessions, hashKey(token))
	_ = a.saveLocked()
}

// EndAllSessions signs out every browser.
func (a *Auth) EndAllSessions() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.data.Sessions = map[string]time.Time{}
	_ = a.saveLocked()
}

func (a *Auth) Passkeys() []Passkey {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]Passkey{}, a.data.Passkeys...)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (a *Auth) DeletePasskey(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, p := range a.data.Passkeys {
		if p.ID == id {
			a.data.Passkeys = append(a.data.Passkeys[:i], a.data.Passkeys[i+1:]...)
			break
		}
	}
	_ = a.saveLocked()
}

func (a *Auth) challenge(purpose string) string {
	c := b64url(randomBytes(32))
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for k, v := range a.challenges {
		if v.expires.Before(now) {
			delete(a.challenges, k)
		}
	}
	if len(a.challenges) > 1000 {
		a.challenges = map[string]authChallenge{}
	}
	a.challenges[c] = authChallenge{purpose: purpose, expires: now.Add(5 * time.Minute)}
	return c
}

func (a *Auth) consumeChallenge(c, purpose string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch, ok := a.challenges[c]
	delete(a.challenges, c)
	return ok && ch.purpose == purpose && ch.expires.After(a.now())
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64url(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// relyingParty is the WebAuthn RP ID (host name) and the origin pages are served from.
func (s *Server) relyingParty(r *http.Request) (rpID, origin string) {
	if s.cfg.PublicURL != "" {
		if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Host != "" {
			return u.Hostname(), u.Scheme + "://" + u.Host
		}
	}
	scheme := "http"
	if r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https") {
		scheme = "https"
	}
	host := r.Host
	h := host
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.Trim(h, "[]"), scheme + "://" + host
}

type clientData struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Origin    string `json:"origin"`
}

func (s *Server) checkClientData(r *http.Request, raw []byte, wantType, purpose string) error {
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return errors.New("bad clientDataJSON")
	}
	if cd.Type != wantType {
		return fmt.Errorf("clientData type %q", cd.Type)
	}
	if !s.auth.consumeChallenge(cd.Challenge, purpose) {
		return errors.New("the challenge expired or was already used; try again")
	}
	if _, origin := s.relyingParty(r); cd.Origin != origin {
		return fmt.Errorf("origin %q doesn't match %q (set SNAG_PUBLIC_URL to the address you sign in at)", cd.Origin, origin)
	}
	return nil
}

const (
	flagUserPresent  = 0x01
	flagUserVerified = 0x04
	flagAttestedData = 0x40
)

// RegisterPasskey verifies a navigator.credentials.create() response ("none" attestation).
func (s *Server) RegisterPasskey(r *http.Request, name string, clientDataJSON, attestationObject []byte) (*Passkey, error) {
	if err := s.checkClientData(r, clientDataJSON, "webauthn.create", "register"); err != nil {
		return nil, err
	}
	obj, err := decodeCBOR(attestationObject)
	if err != nil {
		return nil, fmt.Errorf("attestationObject: %w", err)
	}
	m, _ := obj.(map[string]any)
	authData, _ := m["authData"].([]byte)
	rpID, _ := s.relyingParty(r)
	rpHash := sha256.Sum256([]byte(rpID))
	if len(authData) < 37+16+2 || !bytes.Equal(authData[:32], rpHash[:]) {
		return nil, errors.New("the passkey is for a different site")
	}
	flags := authData[32]
	if flags&flagUserPresent == 0 || flags&flagAttestedData == 0 {
		return nil, errors.New("no user presence or credential data")
	}
	signCount := binary.BigEndian.Uint32(authData[33:37])
	rest := authData[37+16:]
	idLen := int(binary.BigEndian.Uint16(rest[:2]))
	if len(rest) < 2+idLen {
		return nil, errors.New("truncated credential")
	}
	credID := rest[2 : 2+idLen]
	key, _, err := decodeCBORPrefix(rest[2+idLen:])
	if err != nil {
		return nil, fmt.Errorf("credential key: %w", err)
	}
	cose, _ := key.(map[string]any)
	// kty 2 (EC2), alg -7 (ES256), crv 1 (P-256)
	if fmt.Sprint(cose["1"]) != "2" || fmt.Sprint(cose["3"]) != "-7" || fmt.Sprint(cose["-1"]) != "1" {
		return nil, errors.New("only ES256 passkeys are supported")
	}
	x, _ := cose["-2"].([]byte)
	y, _ := cose["-3"].([]byte)
	if len(x) != 32 || len(y) != 32 || !elliptic.P256().IsOnCurve(new(big.Int).SetBytes(x), new(big.Int).SetBytes(y)) {
		return nil, errors.New("bad public key")
	}
	name = truncate(strings.TrimSpace(name), 60)
	if name == "" {
		name = "Passkey " + time.Now().UTC().Format("2006-01-02")
	}
	pk := Passkey{ID: b64url(credID), Name: name, X: x, Y: y, SignCount: signCount, CreatedAt: s.auth.now().UTC()}
	a := s.auth
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, existing := range a.data.Passkeys {
		if existing.ID == pk.ID {
			return nil, errors.New("this passkey is already registered")
		}
	}
	a.data.Passkeys = append(a.data.Passkeys, pk)
	return &pk, a.saveLocked()
}

// VerifyPasskeyLogin checks a navigator.credentials.get() response.
func (s *Server) VerifyPasskeyLogin(r *http.Request, credID string, clientDataJSON, authData, signature []byte) error {
	if err := s.checkClientData(r, clientDataJSON, "webauthn.get", "login"); err != nil {
		return err
	}
	rpID, _ := s.relyingParty(r)
	rpHash := sha256.Sum256([]byte(rpID))
	if len(authData) < 37 || !bytes.Equal(authData[:32], rpHash[:]) {
		return errors.New("the passkey is for a different site")
	}
	if authData[32]&flagUserPresent == 0 {
		return errors.New("no user presence")
	}
	count := binary.BigEndian.Uint32(authData[33:37])
	a := s.auth
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.data.Passkeys {
		pk := &a.data.Passkeys[i]
		if subtle.ConstantTimeCompare([]byte(pk.ID), []byte(credID)) != 1 {
			continue
		}
		pub := ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(pk.X), Y: new(big.Int).SetBytes(pk.Y)}
		cdHash := sha256.Sum256(clientDataJSON)
		digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
		if !ecdsa.VerifyASN1(&pub, digest[:], signature) {
			return errors.New("the signature doesn't verify")
		}
		// Counters only ever grow; synced passkeys always report 0.
		if count != 0 || pk.SignCount != 0 {
			if count <= pk.SignCount {
				return errors.New("the passkey's counter went backwards (a cloned key?)")
			}
		}
		pk.SignCount = count
		pk.LastUsedAt = a.now().UTC()
		return a.saveLocked()
	}
	return errors.New("unknown passkey")
}

// Handlers

func (s *Server) sessionToken(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) error {
	token, err := s.auth.NewSession()
	if err != nil {
		return err
	}
	_, origin := s.relyingParty(r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionLifetime / time.Second),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(origin, "https://")})
	return nil
}

// safeNext keeps redirects after sign-in on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") || strings.HasPrefix(next, "/login") {
		return "/"
	}
	return next
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", dashboardCSP)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	data := map[string]any{"Next": safeNext(r.FormValue("next")), "Error": message, "HasPasskeys": len(s.auth.Passkeys()) > 0,
		"Disabled": s.cfg.AdminPassword == ""}
	if err := templates.ExecuteTemplate(w, "login.html", data); err != nil {
		s.log.Error("rendering login", "err", err)
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth.Valid(s.sessionToken(r)) {
		http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, http.StatusOK, "")
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AdminPassword == "" {
		s.renderLogin(w, r, http.StatusForbidden, "")
		return
	}
	if !s.limiter.allow("login|" + s.clientIP(r)) {
		s.renderLogin(w, r, http.StatusTooManyRequests, "Too many attempts. Try again later.")
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(r.FormValue("user")), []byte(s.cfg.AdminUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(s.cfg.AdminPassword)) == 1
	if !userOK || !passOK {
		time.Sleep(300 * time.Millisecond)
		s.renderLogin(w, r, http.StatusUnauthorized, "Wrong user name or password.")
		return
	}
	if err := s.startSession(w, r); err != nil {
		s.renderLogin(w, r, http.StatusInternalServerError, "Couldn't start a session.")
		return
	}
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := s.sessionToken(r); token != "" {
		s.auth.EndSession(token)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AdminPassword == "" || len(s.auth.Passkeys()) == 0 {
		fail(w, http.StatusNotFound, "no passkeys")
		return
	}
	if !s.limiter.allow("login|" + s.clientIP(r)) {
		fail(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	rpID, _ := s.relyingParty(r)
	var allow []map[string]any
	for _, pk := range s.auth.Passkeys() {
		allow = append(allow, map[string]any{"type": "public-key", "id": pk.ID})
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"challenge": s.auth.challenge("login"), "rpId": rpID, "allowCredentials": allow,
		"userVerification": "preferred", "timeout": 120000,
	})
}

func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AdminPassword == "" {
		fail(w, http.StatusForbidden, "the dashboard is disabled")
		return
	}
	var body struct {
		ID, ClientDataJSON, AuthenticatorData, Signature, Next string
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	cd, err1 := unb64url(body.ClientDataJSON)
	ad, err2 := unb64url(body.AuthenticatorData)
	sig, err3 := unb64url(body.Signature)
	if err1 != nil || err2 != nil || err3 != nil {
		fail(w, http.StatusBadRequest, "bad encoding")
		return
	}
	if err := s.VerifyPasskeyLogin(r, body.ID, cd, ad, sig); err != nil {
		s.log.Warn("passkey sign-in failed", "err", err)
		fail(w, http.StatusUnauthorized, "%v", err)
		return
	}
	if err := s.startSession(w, r); err != nil {
		fail(w, http.StatusInternalServerError, "couldn't start a session")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "next": safeNext(body.Next)})
}

// Account page: passkeys and sessions.

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	rpID, _ := s.relyingParty(r)
	s.render(w, "account.html", map[string]any{"Passkeys": s.auth.Passkeys(), "RPID": rpID, "Flash": r.URL.Query().Get("flash"),
		"BasicOnly": s.sessionToken(r) == ""})
}

// csrfHeader protects the JSON account endpoints: the token comes in X-Snag-CSRF.
func (s *Server) csrfHeader(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Snag-CSRF")), []byte(s.csrfToken())) != 1 {
			fail(w, http.StatusForbidden, "invalid form token; reload the page")
			return
		}
		next(w, r)
	}
}

func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	rpID, _ := s.relyingParty(r)
	var exclude []map[string]any
	for _, pk := range s.auth.Passkeys() {
		exclude = append(exclude, map[string]any{"type": "public-key", "id": pk.ID})
	}
	s.auth.mu.Lock()
	userID := b64url(s.auth.data.UserID)
	s.auth.mu.Unlock()
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"challenge":              s.auth.challenge("register"),
		"rp":                     map[string]any{"id": rpID, "name": "Snag"},
		"user":                   map[string]any{"id": userID, "name": s.cfg.AdminUser, "displayName": s.cfg.AdminUser + " (Snag)"},
		"pubKeyCredParams":       []map[string]any{{"type": "public-key", "alg": -7}},
		"excludeCredentials":     exclude,
		"authenticatorSelection": map[string]any{"residentKey": "preferred", "userVerification": "preferred"},
		"attestation":            "none", "timeout": 120000,
	})
}

func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name, ClientDataJSON, AttestationObject string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	cd, err1 := unb64url(body.ClientDataJSON)
	ao, err2 := unb64url(body.AttestationObject)
	if err1 != nil || err2 != nil {
		fail(w, http.StatusBadRequest, "bad encoding")
		return
	}
	pk, err := s.RegisterPasskey(r, body.Name, cd, ao)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	s.log.Info("passkey added", "name", pk.Name)
	writeJSONResponse(w, http.StatusCreated, map[string]any{"id": pk.ID, "name": pk.Name})
}

func (s *Server) handleDeletePasskey(w http.ResponseWriter, r *http.Request) {
	s.auth.DeletePasskey(r.PathValue("id"))
	http.Redirect(w, r, "/account?flash=Passkey+removed", http.StatusSeeOther)
}

func (s *Server) handleSignOutEverywhere(w http.ResponseWriter, r *http.Request) {
	s.auth.EndAllSessions()
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
