package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AttestConfig is a project's App Attest policy.
type AttestConfig struct {
	// Mode is "off" (default), "monitor" (verify when present and mark reports) or "require"
	// (reject reports that don't prove they come from your app on a real device).
	Mode string `json:"mode,omitempty"`
	// AppIDs are "<Team ID>.<bundle ID>" values allowed to report, e.g. "ABCDE12345.com.example.app".
	AppIDs []string `json:"appIds,omitempty"`
	// AllowDevelopment accepts keys from development-signed builds (Xcode runs).
	AllowDevelopment bool `json:"allowDevelopment,omitempty"`
}

const (
	AttestOff     = ""
	AttestMonitor = "monitor"
	AttestRequire = "require"
)

//go:embed appattest_root.pem
var appAttestRootPEM []byte

// AttestedKey is an App Attest key that passed attestation.
type AttestedKey struct {
	ID          string    `json:"id"` // base64 key identifier, as the app knows it
	Project     string    `json:"project"`
	AppID       string    `json:"appId"`
	PublicKey   []byte    `json:"publicKey"` // PKIX DER
	Counter     uint32    `json:"counter"`
	Development bool      `json:"development,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	LastUsed    time.Time `json:"lastUsed,omitempty"`
}

// Attestor verifies App Attest attestations and assertions.
type Attestor struct {
	path       string
	roots      *x509.CertPool
	mu         sync.Mutex
	keys       map[string]*AttestedKey
	challenges map[string]challenge
	now        func() time.Time
}

type challenge struct {
	project string
	expires time.Time
}

// NewAttestor loads attested keys from <data>/attest-keys.json. roots defaults to Apple's App
// Attestation Root CA.
func NewAttestor(dataDir string, roots *x509.CertPool) (*Attestor, error) {
	if roots == nil {
		roots = x509.NewCertPool()
		block, _ := pem.Decode(appAttestRootPEM)
		if block == nil {
			return nil, errors.New("embedded App Attest root is invalid")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		roots.AddCert(cert)
	}
	a := &Attestor{path: filepath.Join(dataDir, "attest-keys.json"), roots: roots, keys: map[string]*AttestedKey{}, challenges: map[string]challenge{}, now: time.Now}
	if data, err := os.ReadFile(a.path); err == nil {
		var list []*AttestedKey
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("attest-keys.json: %w", err)
		}
		for _, k := range list {
			a.keys[k.ID] = k
		}
	}
	return a, nil
}

func (a *Attestor) saveLocked() error {
	list := make([]*AttestedKey, 0, len(a.keys))
	for _, k := range a.keys {
		list = append(list, k)
	}
	return writeJSON(a.path, list)
}

// Challenge issues a one-time challenge for a project, valid for five minutes.
func (a *Attestor) Challenge(project string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	c := base64.StdEncoding.EncodeToString(b)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for k, v := range a.challenges {
		if now.After(v.expires) {
			delete(a.challenges, k)
		}
	}
	if len(a.challenges) > 10_000 {
		return "" // flooded; callers retry later
	}
	a.challenges[c] = challenge{project: project, expires: now.Add(5 * time.Minute)}
	return c
}

func (a *Attestor) consume(project, c string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.challenges[c]
	delete(a.challenges, c)
	return ok && v.project == project && a.now().Before(v.expires)
}

var (
	oidAppAttestNonce = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 8, 2}
	aaguidProduction  = append([]byte("appattest"), make([]byte, 7)...)
	aaguidDevelopment = []byte("appattestdevelop")
)

type authData struct {
	rpIDHash []byte
	flags    byte
	counter  uint32
	aaguid   []byte
	credID   []byte
}

func parseAuthData(b []byte, attested bool) (authData, error) {
	if len(b) < 37 {
		return authData{}, errors.New("authenticator data is too short")
	}
	ad := authData{rpIDHash: b[:32], flags: b[32], counter: binary.BigEndian.Uint32(b[33:37])}
	if attested {
		if len(b) < 55 {
			return authData{}, errors.New("authenticator data has no credential")
		}
		ad.aaguid = b[37:53]
		n := int(binary.BigEndian.Uint16(b[53:55]))
		if len(b) < 55+n {
			return authData{}, errors.New("credential ID runs past the end")
		}
		ad.credID = b[55 : 55+n]
	}
	return ad, nil
}

// Attest verifies an attestation object for keyID (base64) against challenge and stores the
// key for the project. It follows Apple's "Validating apps that connect to your server".
func (a *Attestor) Attest(p *Project, keyID string, attestation []byte, challengeValue string) (*AttestedKey, error) {
	if !a.consume(p.Slug, challengeValue) {
		return nil, errors.New("the challenge is unknown, used or expired")
	}
	keyIDBytes, err := base64.StdEncoding.DecodeString(keyID)
	if err != nil || len(keyIDBytes) != 32 {
		return nil, errors.New("keyId must be the base64 key identifier")
	}
	decoded, err := decodeCBOR(attestation)
	if err != nil {
		return nil, err
	}
	obj, _ := decoded.(map[string]any)
	if obj == nil || obj["fmt"] != "apple-appattest" {
		return nil, errors.New("not an apple-appattest attestation")
	}
	stmt, _ := obj["attStmt"].(map[string]any)
	rawAuth, _ := obj["authData"].([]byte)
	x5c, _ := stmt["x5c"].([]any)
	if len(x5c) < 2 || rawAuth == nil {
		return nil, errors.New("attestation is missing certificates or authenticator data")
	}
	// 1. The credential certificate chains to Apple's App Attestation root.
	var certs []*x509.Certificate
	for _, c := range x5c {
		der, _ := c.([]byte)
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{Roots: a.roots, Intermediates: intermediates, CurrentTime: a.now(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return nil, fmt.Errorf("certificate chain: %w", err)
	}
	// 2–4. The nonce in the certificate is SHA256(authData ‖ SHA256(challenge)).
	clientDataHash := sha256.Sum256([]byte(challengeValue))
	nonce := sha256.Sum256(append(append([]byte{}, rawAuth...), clientDataHash[:]...))
	var certNonce []byte
	for _, ext := range certs[0].Extensions {
		if ext.Id.Equal(oidAppAttestNonce) {
			var seq asn1.RawValue
			if _, err := asn1.Unmarshal(ext.Value, &seq); err != nil {
				return nil, errors.New("nonce extension is malformed")
			}
			var tagged asn1.RawValue
			if _, err := asn1.Unmarshal(seq.Bytes, &tagged); err != nil {
				return nil, errors.New("nonce extension is malformed")
			}
			if _, err := asn1.Unmarshal(tagged.Bytes, &certNonce); err != nil {
				return nil, errors.New("nonce extension is malformed")
			}
		}
	}
	if !bytes.Equal(certNonce, nonce[:]) {
		return nil, errors.New("nonce doesn't match")
	}
	// 5. The key identifier is the SHA256 of the credential public key.
	pub, ok := certs[0].PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("credential key isn't ECDSA")
	}
	point, err := pub.ECDH()
	if err != nil {
		return nil, err
	}
	keyHash := sha256.Sum256(point.Bytes())
	if !bytes.Equal(keyHash[:], keyIDBytes) {
		return nil, errors.New("key identifier doesn't match the credential")
	}
	// 6–9. App ID, counter, environment and credential ID.
	ad, err := parseAuthData(rawAuth, true)
	if err != nil {
		return nil, err
	}
	appID := matchAppID(p.Attest.AppIDs, ad.rpIDHash)
	if appID == "" {
		return nil, errors.New("the app ID isn't one this project allows")
	}
	if ad.counter != 0 {
		return nil, errors.New("a new key's counter must be zero")
	}
	development := bytes.Equal(ad.aaguid, aaguidDevelopment)
	switch {
	case bytes.Equal(ad.aaguid, aaguidProduction):
	case development && p.Attest.AllowDevelopment:
	case development:
		return nil, errors.New("development keys aren't allowed for this project")
	default:
		return nil, errors.New("unknown App Attest environment")
	}
	if !bytes.Equal(ad.credID, keyIDBytes) {
		return nil, errors.New("credential ID doesn't match the key identifier")
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	key := &AttestedKey{ID: keyID, Project: p.Slug, AppID: appID, PublicKey: der, Development: development, CreatedAt: a.now().UTC()}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.keys[keyID] = key
	if err := a.saveLocked(); err != nil {
		return nil, err
	}
	return key, nil
}

func matchAppID(allowed []string, rpIDHash []byte) string {
	for _, id := range allowed {
		h := sha256.Sum256([]byte(strings.TrimSpace(id)))
		if bytes.Equal(h[:], rpIDHash) {
			return id
		}
	}
	return ""
}

// VerifyAssertion checks an assertion made with an attested key of the project over
// clientDataHash (SHA256 of the request body), and advances the key's counter.
func (a *Attestor) VerifyAssertion(p *Project, keyID string, assertion, clientDataHash []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	key, ok := a.keys[keyID]
	if !ok || key.Project != p.Slug {
		return errors.New("unknown attestation key")
	}
	decoded, err := decodeCBOR(assertion)
	if err != nil {
		return err
	}
	obj, _ := decoded.(map[string]any)
	sig, _ := obj["signature"].([]byte)
	rawAuth, _ := obj["authenticatorData"].([]byte)
	if sig == nil || rawAuth == nil {
		return errors.New("assertion is missing its signature or authenticator data")
	}
	nonce := sha256.Sum256(append(append([]byte{}, rawAuth...), clientDataHash...))
	pubAny, err := x509.ParsePKIXPublicKey(key.PublicKey)
	if err != nil {
		return err
	}
	pub := pubAny.(*ecdsa.PublicKey)
	digest := sha256.Sum256(nonce[:])
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		return errors.New("assertion signature is invalid")
	}
	ad, err := parseAuthData(rawAuth, false)
	if err != nil {
		return err
	}
	if h := sha256.Sum256([]byte(key.AppID)); !bytes.Equal(ad.rpIDHash, h[:]) {
		return errors.New("assertion is for a different app")
	}
	if ad.counter <= key.Counter {
		return errors.New("assertion counter didn't increase (replayed?)")
	}
	key.Counter = ad.counter
	key.LastUsed = a.now().UTC()
	return a.saveLocked()
}

// MARK: HTTP

func (s *Server) handleAttestChallenge(w http.ResponseWriter, r *http.Request) {
	project, ok := s.authenticateProject(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "missing or invalid API key")
		return
	}
	if project.Attest.Mode == AttestOff {
		fail(w, http.StatusNotFound, "App Attest isn't enabled for this project")
		return
	}
	if !s.limiter.allow("attest|" + s.clientIP(r)) {
		fail(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	c := s.attestor.Challenge(project.Slug)
	if c == "" {
		fail(w, http.StatusServiceUnavailable, "try again later")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{"challenge": c})
}

func (s *Server) handleAttestKey(w http.ResponseWriter, r *http.Request) {
	project, ok := s.authenticateProject(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "missing or invalid API key")
		return
	}
	if project.Attest.Mode == AttestOff {
		fail(w, http.StatusNotFound, "App Attest isn't enabled for this project")
		return
	}
	var body struct {
		KeyID       string `json:"keyId"`
		Attestation string `json:"attestation"`
		Challenge   string `json:"challenge"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	attestation, err := base64.StdEncoding.DecodeString(body.Attestation)
	if err != nil {
		fail(w, http.StatusBadRequest, "attestation must be base64")
		return
	}
	key, err := s.attestor.Attest(project, body.KeyID, attestation, body.Challenge)
	if err != nil {
		s.log.Warn("attestation rejected", "project", project.Slug, "err", err)
		fail(w, http.StatusForbidden, "attestation rejected: %v", err)
		return
	}
	s.log.Info("device attested", "project", project.Slug, "app", key.AppID, "development", key.Development)
	writeJSONResponse(w, http.StatusCreated, map[string]any{"ok": true})
}
