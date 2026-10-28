package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPasswordSignInAndSessions(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(t))
	login := func(pw, next string) *httptest.ResponseRecorder {
		form := url.Values{"user": {"admin"}, "password": {pw}, "next": {next}}
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	if rr := login("wrong", "/"); rr.Code != http.StatusUnauthorized || len(rr.Result().Cookies()) != 0 {
		t.Fatalf("wrong password: %d", rr.Code)
	}
	rr := login("pw-for-tests", "//evil.com/x")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/" {
		t.Fatalf("login: %d %s (open redirect?)", rr.Code, rr.Header().Get("Location"))
	}
	cookie := rr.Result().Cookies()[0]
	if cookie.Name != sessionCookie || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie %+v", cookie)
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	if rr := get("/p/gelo"); rr.Code != 200 {
		t.Fatalf("with session: %d", rr.Code)
	}
	if rr := login("pw-for-tests", "/p/gelo/settings"); rr.Header().Get("Location") != "/p/gelo/settings" {
		t.Fatalf("next: %s", rr.Header().Get("Location"))
	}
	// Signing out ends that session.
	form := url.Values{"csrf": {srv.csrfToken()}}
	req := httptest.NewRequest("POST", "/logout", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	srv.ServeHTTP(httptest.NewRecorder(), req)
	if rr := get("/p/gelo"); rr.Code != http.StatusSeeOther {
		t.Fatalf("after logout: %d", rr.Code)
	}
	// Sessions survive a restart.
	rr = login("pw-for-tests", "/")
	cookie = rr.Result().Cookies()[0]
	again, _ := newTestServer(t, srv.cfg)
	req = httptest.NewRequest("GET", "/p/gelo", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	again.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("after restart: %d", rr.Code)
	}
}

// fakeAuthenticator plays a platform authenticator with an ES256 key.
type fakeAuthenticator struct {
	key    *ecdsa.PrivateKey
	id     []byte
	rpID   string
	origin string
	count  uint32
}

func (f *fakeAuthenticator) authData(withCredential bool) []byte {
	rp := sha256.Sum256([]byte(f.rpID))
	out := append([]byte{}, rp[:]...)
	flags := byte(flagUserPresent | flagUserVerified)
	if withCredential {
		flags |= flagAttestedData
	}
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, f.count)
	if withCredential {
		out = append(out, make([]byte, 16)...) // aaguid
		out = binary.BigEndian.AppendUint16(out, uint16(len(f.id)))
		out = append(out, f.id...)
		x, y := make([]byte, 32), make([]byte, 32)
		f.key.X.FillBytes(x)
		f.key.Y.FillBytes(y)
		// COSE key {1: 2, 3: -7, -1: 1, -2: x, -3: y}
		cose := []byte{0xa5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20}
		cose = append(cose, x...)
		cose = append(cose, 0x22, 0x58, 0x20)
		cose = append(cose, y...)
		out = append(out, cose...)
	}
	return out
}

func (f *fakeAuthenticator) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]string{"type": typ, "challenge": challenge, "origin": f.origin})
	return b
}

func TestPasskeyRegistrationAndSignIn(t *testing.T) {
	cfg := testConfig(t)
	cfg.PublicURL = "https://snag.example.com"
	srv, _ := newTestServer(t, cfg)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	auth := &fakeAuthenticator{key: key, id: []byte("credential-1"), rpID: "snag.example.com", origin: "https://snag.example.com"}

	jsonPost := func(path string, body any, admin bool) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", path, strings.NewReader(string(data)))
		req.Header.Set("Content-Type", "application/json")
		if admin {
			req.SetBasicAuth("admin", "pw-for-tests")
			req.Header.Set("X-Snag-CSRF", srv.csrfToken())
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	if rr := jsonPost("/account/passkeys/begin", nil, false); rr.Code == 200 {
		t.Fatal("registration without signing in")
	}
	var opts struct {
		Challenge string `json:"challenge"`
		RP        struct{ ID string } `json:"rp"`
	}
	_ = json.Unmarshal(jsonPost("/account/passkeys/begin", nil, true).Body.Bytes(), &opts)
	if opts.RP.ID != "snag.example.com" || opts.Challenge == "" {
		t.Fatalf("options %+v", opts)
	}
	// attestationObject: {"fmt": "none", "attStmt": {}, "authData": <bytes>}
	ad := auth.authData(true)
	att := append([]byte{0xa3, 0x63, 'f', 'm', 't', 0x64, 'n', 'o', 'n', 'e', 0x67, 'a', 't', 't', 'S', 't', 'm', 't', 0xa0,
		0x68, 'a', 'u', 't', 'h', 'D', 'a', 't', 'a', 0x59}, byte(len(ad)>>8), byte(len(ad)))
	att = append(att, ad...)
	rr := jsonPost("/account/passkeys/finish", map[string]string{"name": "MacBook",
		"clientDataJSON": b64url(auth.clientData("webauthn.create", opts.Challenge)), "attestationObject": b64url(att)}, true)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body)
	}
	// The challenge is single use.
	if rr := jsonPost("/account/passkeys/finish", map[string]string{"clientDataJSON": b64url(auth.clientData("webauthn.create", opts.Challenge)), "attestationObject": b64url(att)}, true); rr.Code == http.StatusCreated {
		t.Fatal("challenge reused")
	}
	if !strings.Contains(adminGet(srv, "/login").Body.String(), "Sign in with a passkey") {
		t.Error("login page doesn't offer the passkey")
	}

	signIn := func(mutate func(cd, ad []byte) ([]byte, []byte)) *httptest.ResponseRecorder {
		var begin struct {
			Challenge string `json:"challenge"`
		}
		_ = json.Unmarshal(jsonPost("/login/passkey/begin", nil, false).Body.Bytes(), &begin)
		auth.count++
		cd, ad := auth.clientData("webauthn.get", begin.Challenge), auth.authData(false)
		if mutate != nil {
			cd, ad = mutate(cd, ad)
		}
		h := sha256.Sum256(cd)
		digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		return jsonPost("/login/passkey/finish", map[string]string{"id": b64url(auth.id), "clientDataJSON": b64url(cd),
			"authenticatorData": b64url(ad), "signature": b64url(sig), "next": "/p/gelo"}, false)
	}
	rr = signIn(nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"next":"/p/gelo"`) || len(rr.Result().Cookies()) != 1 {
		t.Fatalf("passkey sign-in: %d %s", rr.Code, rr.Body)
	}
	req := httptest.NewRequest("GET", "/p/gelo", nil)
	req.AddCookie(rr.Result().Cookies()[0])
	page := httptest.NewRecorder()
	srv.ServeHTTP(page, req)
	if page.Code != 200 {
		t.Fatalf("session from passkey: %d", page.Code)
	}
	// Wrong origin, and a counter that goes backwards, are refused.
	if rr := signIn(func(cd, ad []byte) ([]byte, []byte) {
		return []byte(strings.Replace(string(cd), "https://snag.example.com", "https://evil.example.com", 1)), ad
	}); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong origin: %d", rr.Code)
	}
	auth.count = 0
	if rr := signIn(nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("counter went backwards: %d", rr.Code)
	}
	if !strings.Contains(adminGet(srv, "/account").Body.String(), "MacBook") {
		t.Error("account page doesn't list the passkey")
	}
}
