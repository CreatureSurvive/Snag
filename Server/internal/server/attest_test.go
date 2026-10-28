package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

// encodeCBOR is the inverse of decodeCBOR, for building test attestations.
func encodeCBOR(v any) []byte {
	var b bytes.Buffer
	head := func(major byte, n uint64) {
		switch {
		case n < 24:
			b.WriteByte(major<<5 | byte(n))
		case n < 1<<8:
			b.WriteByte(major<<5 | 24)
			b.WriteByte(byte(n))
		case n < 1<<16:
			b.WriteByte(major<<5 | 25)
			_ = binary.Write(&b, binary.BigEndian, uint16(n))
		default:
			b.WriteByte(major<<5 | 26)
			_ = binary.Write(&b, binary.BigEndian, uint32(n))
		}
	}
	var enc func(any)
	enc = func(v any) {
		switch x := v.(type) {
		case int:
			head(0, uint64(x))
		case []byte:
			head(2, uint64(len(x)))
			b.Write(x)
		case string:
			head(3, uint64(len(x)))
			b.WriteString(x)
		case []any:
			head(4, uint64(len(x)))
			for _, e := range x {
				enc(e)
			}
		case map[string]any:
			head(5, uint64(len(x)))
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				enc(k)
				enc(x[k])
			}
		}
	}
	enc(v)
	return b.Bytes()
}

func TestCBORRoundTrip(t *testing.T) {
	in := map[string]any{"fmt": "x", "n": 300, "list": []any{[]byte{1, 2}, "a"}, "big": bytes.Repeat([]byte{7}, 70000)}
	out, err := decodeCBOR(encodeCBOR(in))
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["fmt"] != "x" || m["n"] != uint64(300) || len(m["big"].([]byte)) != 70000 || m["list"].([]any)[1] != "a" {
		t.Fatalf("round trip %v", m["list"])
	}
	for _, bad := range [][]byte{{0x5f}, {0x45, 1}, {0x82, 1}, {0xa1, 0x81, 1, 2}, append(encodeCBOR("a"), 0)} {
		if _, err := decodeCBOR(bad); err == nil {
			t.Errorf("accepted % x", bad)
		}
	}
}

// fakeApple mints App Attest attestations signed by a test root.
type fakeApple struct {
	root     *x509.Certificate
	rootKey  *ecdsa.PrivateKey
	inter    *x509.Certificate
	interKey *ecdsa.PrivateKey
	pool     *x509.CertPool
}

func newFakeApple(t *testing.T) *fakeApple {
	f := &fakeApple{}
	f.rootKey, _ = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	f.interKey, _ = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	mk := func(tmpl *x509.Certificate, parent *x509.Certificate, pub any, signer *ecdsa.PrivateKey) *x509.Certificate {
		if parent == nil {
			parent = tmpl
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		return c
	}
	now := time.Now()
	f.root = mk(&x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test App Attestation Root CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}, nil, &f.rootKey.PublicKey, f.rootKey)
	f.inter = mk(&x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Test App Attestation CA 1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}, f.root, &f.interKey.PublicKey, f.rootKey)
	f.pool = x509.NewCertPool()
	f.pool.AddCert(f.root)
	return f
}

type deviceKey struct {
	priv  *ecdsa.PrivateKey
	id    []byte // SHA256 of the public point
	appID string
	count uint32
}

func newDeviceKey(appID string) *deviceKey {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	point, _ := priv.PublicKey.ECDH()
	id := sha256.Sum256(point.Bytes())
	return &deviceKey{priv: priv, id: id[:], appID: appID}
}

func (k *deviceKey) keyID() string { return base64.StdEncoding.EncodeToString(k.id) }

type attestOpts struct {
	aaguid    []byte
	counter   uint32
	appID     string
	badNonce  bool
	otherCred bool
}

func (f *fakeApple) attest(t *testing.T, k *deviceKey, challenge string, o attestOpts) []byte {
	appID := k.appID
	if o.appID != "" {
		appID = o.appID
	}
	aaguid := o.aaguid
	if aaguid == nil {
		aaguid = aaguidProduction
	}
	rp := sha256.Sum256([]byte(appID))
	var auth bytes.Buffer
	auth.Write(rp[:])
	auth.WriteByte(0x41)
	_ = binary.Write(&auth, binary.BigEndian, o.counter)
	auth.Write(aaguid)
	cred := k.id
	if o.otherCred {
		cred = bytes.Repeat([]byte{9}, 32)
	}
	_ = binary.Write(&auth, binary.BigEndian, uint16(len(cred)))
	auth.Write(cred)
	auth.Write(encodeCBOR(map[string]any{"cose": "key"}))
	clientDataHash := sha256.Sum256([]byte(challenge))
	nonce := sha256.Sum256(append(auth.Bytes(), clientDataHash[:]...))
	if o.badNonce {
		nonce[0] ^= 1
	}
	octet, _ := asn1.Marshal(nonce[:])
	tagged, _ := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 1, IsCompound: true, Bytes: octet})
	seq, _ := asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: tagged})
	now := time.Now()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: hexID(k.id)},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtraExtensions: []pkix.Extension{{Id: oidAppAttestNonce, Value: seq}},
	}, f.inter, &k.priv.PublicKey, f.interKey)
	if err != nil {
		t.Fatal(err)
	}
	return encodeCBOR(map[string]any{
		"fmt":      "apple-appattest",
		"attStmt":  map[string]any{"x5c": []any{der, f.inter.Raw}, "receipt": []byte("receipt")},
		"authData": auth.Bytes(),
	})
}

func hexID(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// assert makes an assertion over clientDataHash, as DCAppAttestService.generateAssertion does.
func (k *deviceKey) assert(clientDataHash []byte, counterOverride ...uint32) []byte {
	k.count++
	counter := k.count
	if len(counterOverride) > 0 {
		counter = counterOverride[0]
	}
	rp := sha256.Sum256([]byte(k.appID))
	var auth bytes.Buffer
	auth.Write(rp[:])
	auth.WriteByte(0x01)
	_ = binary.Write(&auth, binary.BigEndian, counter)
	nonce := sha256.Sum256(append(auth.Bytes(), clientDataHash...))
	digest := sha256.Sum256(nonce[:])
	sig, _ := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	return encodeCBOR(map[string]any{"signature": sig, "authenticatorData": auth.Bytes()})
}

const testAppID = "ABCDE12345.com.example.gelo"

func attestProject(mode string) *Project {
	return &Project{Slug: "gelo", Attest: AttestConfig{Mode: mode, AppIDs: []string{testAppID}}}
}

func TestAttestationChecks(t *testing.T) {
	apple := newFakeApple(t)
	a, err := NewAttestor(t.TempDir(), apple.pool)
	if err != nil {
		t.Fatal(err)
	}
	p := attestProject(AttestRequire)
	attest := func(k *deviceKey, o attestOpts) error {
		c := a.Challenge(p.Slug)
		_, err := a.Attest(p, k.keyID(), apple.attest(t, k, c, o), c)
		return err
	}
	k := newDeviceKey(testAppID)
	if err := attest(k, attestOpts{}); err != nil {
		t.Fatalf("valid attestation rejected: %v", err)
	}
	cases := map[string]struct {
		o    attestOpts
		want string
	}{
		"nonce":       {attestOpts{badNonce: true}, "nonce"},
		"app id":      {attestOpts{appID: "ABCDE12345.com.other"}, "app ID"},
		"counter":     {attestOpts{counter: 3}, "counter"},
		"development": {attestOpts{aaguid: aaguidDevelopment}, "development"},
		"environment": {attestOpts{aaguid: bytes.Repeat([]byte("x"), 16)}, "environment"},
		"credential":  {attestOpts{otherCred: true}, "credential ID"},
	}
	for name, c := range cases {
		if err := attest(newDeviceKey(testAppID), c.o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	// Development keys when allowed.
	p.Attest.AllowDevelopment = true
	if err := attest(newDeviceKey(testAppID), attestOpts{aaguid: aaguidDevelopment}); err != nil {
		t.Fatalf("development allowed: %v", err)
	}
	// Challenges are single use, per project, and expire.
	c := a.Challenge("gelo")
	k2 := newDeviceKey(testAppID)
	obj := apple.attest(t, k2, c, attestOpts{})
	if _, err := a.Attest(p, k2.keyID(), obj, c); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Attest(p, k2.keyID(), obj, c); err == nil || !strings.Contains(err.Error(), "challenge") {
		t.Fatalf("challenge reuse: %v", err)
	}
	other := a.Challenge("other")
	if _, err := a.Attest(p, k2.keyID(), apple.attest(t, k2, other, attestOpts{}), other); err == nil {
		t.Fatal("another project's challenge accepted")
	}
	old := a.Challenge("gelo")
	a.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	if _, err := a.Attest(p, k2.keyID(), apple.attest(t, k2, old, attestOpts{}), old); err == nil {
		t.Fatal("expired challenge accepted")
	}
	a.now = time.Now
	// The key ID must match the certificate's key.
	c = a.Challenge("gelo")
	if _, err := a.Attest(p, newDeviceKey(testAppID).keyID(), apple.attest(t, newDeviceKey(testAppID), c, attestOpts{}), c); err == nil {
		t.Fatal("mismatched key ID accepted")
	}
	// The real Apple root doesn't trust the test chain.
	real, _ := NewAttestor(t.TempDir(), nil)
	c = real.Challenge("gelo")
	k3 := newDeviceKey(testAppID)
	if _, err := real.Attest(p, k3.keyID(), apple.attest(t, k3, c, attestOpts{}), c); err == nil || !strings.Contains(err.Error(), "certificate chain") {
		t.Fatalf("untrusted chain: %v", err)
	}

	// Assertions.
	hash := sha256.Sum256([]byte("body"))
	if err := a.VerifyAssertion(p, k.keyID(), k.assert(hash[:]), hash[:]); err != nil {
		t.Fatalf("valid assertion: %v", err)
	}
	replay := k.assert(hash[:])
	if err := a.VerifyAssertion(p, k.keyID(), replay, hash[:]); err != nil {
		t.Fatal(err)
	}
	if err := a.VerifyAssertion(p, k.keyID(), replay, hash[:]); err == nil || !strings.Contains(err.Error(), "counter") {
		t.Fatalf("replay: %v", err)
	}
	other2 := sha256.Sum256([]byte("tampered"))
	if err := a.VerifyAssertion(p, k.keyID(), k.assert(hash[:]), other2[:]); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("tampered body: %v", err)
	}
	if err := a.VerifyAssertion(&Project{Slug: "other", Attest: p.Attest}, k.keyID(), k.assert(hash[:]), hash[:]); err == nil {
		t.Fatal("key used by another project")
	}
	// Keys persist.
	reopened, _ := NewAttestor(a.path[:len(a.path)-len("/attest-keys.json")], apple.pool)
	if err := reopened.VerifyAssertion(p, k.keyID(), k.assert(hash[:]), hash[:]); err != nil {
		t.Fatalf("after reload: %v", err)
	}
}

func TestIngestWithAppAttest(t *testing.T) {
	apple := newFakeApple(t)
	cfg := testConfig(t)
	cfg.Seeds[0].Attest = &AttestConfig{Mode: AttestRequire, AppIDs: []string{testAppID}}
	srv, store := newTestServer(t, cfg)
	srv.attestor, _ = NewAttestor(cfg.DataDir, apple.pool)

	call := func(path string, body any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", path, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	var ch struct{ Challenge string }
	_ = json.Unmarshal(call("/api/v1/attest/challenge", nil).Body.Bytes(), &ch)
	k := newDeviceKey(testAppID)
	rr := call("/api/v1/attest/keys", map[string]string{"keyId": k.keyID(), "challenge": ch.Challenge,
		"attestation": base64.StdEncoding.EncodeToString(apple.attest(t, k, ch.Challenge, attestOpts{}))})
	if rr.Code != 201 {
		t.Fatalf("attest: %d %s", rr.Code, rr.Body)
	}

	upload := func(id string, sign func(body []byte) (string, string)) *httptest.ResponseRecorder {
		body, ct := sampleUpload(id).body()
		raw := body.Bytes()
		req := httptest.NewRequest("POST", "/api/v1/reports", bytes.NewReader(raw))
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Authorization", "Bearer "+testKey)
		if sign != nil {
			key, assertion := sign(raw)
			req.Header.Set("X-Snag-Attest-Key", key)
			req.Header.Set("X-Snag-Assertion", assertion)
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	valid := func(body []byte) (string, string) {
		h := sha256.Sum256(body)
		return k.keyID(), base64.StdEncoding.EncodeToString(k.assert(h[:]))
	}
	id := ids(4)
	if rr := upload(id[0], nil); rr.Code != http.StatusForbidden {
		t.Fatalf("unattested: %d", rr.Code)
	}
	if rr := upload(id[1], valid); rr.Code != 201 {
		t.Fatalf("attested: %d %s", rr.Code, rr.Body)
	}
	if rec, _ := store.Get(id[1]); !rec.Attested {
		t.Fatal("not marked attested")
	}
	// An assertion for different bytes (the body was altered) fails.
	if rr := upload(id[2], func(body []byte) (string, string) {
		h := sha256.Sum256(append(body, 'x'))
		return k.keyID(), base64.StdEncoding.EncodeToString(k.assert(h[:]))
	}); rr.Code != http.StatusForbidden {
		t.Fatalf("altered body: %d", rr.Code)
	}
	// Monitor mode accepts but marks.
	_, _ = srv.projects.Update("gelo", func(p *Project) error { p.Attest.Mode = AttestMonitor; return nil })
	if rr := upload(id[3], nil); rr.Code != 201 {
		t.Fatalf("monitor: %d", rr.Code)
	}
	if rec, _ := store.Get(id[3]); rec.Attested {
		t.Fatal("unattested marked attested")
	}
	if _, total := srv.issues.List("gelo", IssueFilter{}); total != 2 {
		t.Fatalf("rejected uploads must not file issues: %d", total)
	}
	page := adminGet(srv, "/p/gelo/issues/1").Body.String() + adminGet(srv, "/p/gelo/issues/2").Body.String()
	if !strings.Contains(page, "✓ verified device") || !strings.Contains(page, ">unverified<") {
		t.Fatal("badges missing")
	}
	// Settings validation.
	if rr := adminPost(t, srv, "/p/gelo/settings", url.Values{"name": {"Gelo"}, "attestMode": {"require"}, "attestAppIDs": {"nope"}}); !strings.Contains(rr.Body.String(), "isn&#39;t an app ID") {
		t.Fatal("bad app ID accepted")
	}
	// Off: endpoints disappear.
	_, _ = srv.projects.Update("gelo", func(p *Project) error { p.Attest.Mode = AttestOff; return nil })
	if rr := call("/api/v1/attest/challenge", nil); rr.Code != 404 {
		t.Fatalf("challenge when off: %d", rr.Code)
	}
}
