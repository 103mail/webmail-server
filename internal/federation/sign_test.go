package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/103mail/webmail-server/internal/store/storetest"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return public, private
}

// Given a signed request, when it is verified with the signer's key, then it
// verifies.
func TestSignRoundTrip(t *testing.T) {
	public, private := testKey(t)
	body := []byte(`{"version":"0.2"}`)
	date := HTTPDate(time.Now())

	sig := Sign(private, Version, "103mail.com", "nonce-1", date, body)
	if !strings.HasPrefix(sig, "ed25519:") {
		t.Fatalf("signature %q does not carry the scheme prefix", sig)
	}
	if !Verify([]ed25519.PublicKey{public}, sig, Version, "103mail.com", "nonce-1", date, body) {
		t.Error("a signature did not verify against the key that made it")
	}
}

// Every field in the signing input has to matter, or a captured request could be
// replayed with one of them changed. This is the check that the concatenation
// is not accidentally ignoring part of its input.
func TestSignatureCoversEveryField(t *testing.T) {
	public, private := testKey(t)
	body := []byte(`{"version":"0.2"}`)
	date := HTTPDate(time.Now())
	sig := Sign(private, Version, "103mail.com", "nonce-1", date, body)

	tests := []struct {
		name                            string
		version, sender, nonce, altDate string
		body                            []byte
	}{
		{"version", "0.3", "103mail.com", "nonce-1", date, body},
		{"sender", Version, "evil.com", "nonce-1", date, body},
		{"nonce", Version, "103mail.com", "nonce-2", date, body},
		{"date", Version, "103mail.com", "nonce-1", HTTPDate(time.Now().Add(time.Hour)), body},
		{"body", Version, "103mail.com", "nonce-1", date, []byte(`{"version":"0.2","x":1}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if Verify([]ed25519.PublicKey{public}, sig, tt.version, tt.sender, tt.nonce, tt.altDate, tt.body) {
				t.Errorf("a changed %s still verified", tt.name)
			}
		})
	}
}

// Given a peer mid-rotation, when it signs with its new key while we still hold
// the old one alongside it, then verification accepts the match.
func TestVerifyAcceptsAnyKeyInTheSet(t *testing.T) {
	oldPublic, _ := testKey(t)
	newPublic, newPrivate := testKey(t)
	body := []byte(`{}`)
	date := HTTPDate(time.Now())
	sig := Sign(newPrivate, Version, "example.com", "n", date, body)

	if !Verify([]ed25519.PublicKey{oldPublic, newPublic}, sig, Version, "example.com", "n", date, body) {
		t.Error("a signature from the new key did not verify against the rotation keyset")
	}
	if Verify([]ed25519.PublicKey{oldPublic}, sig, Version, "example.com", "n", date, body) {
		t.Error("a signature verified against a keyset that does not hold its key")
	}
}

func TestVerifyRejectsMalformedSignatures(t *testing.T) {
	public, _ := testKey(t)
	for _, sig := range []string{"", "notascheme", "ed25519:", "ed25519:!!!", "rsa:AAAA", "ed25519:QUJD"} {
		if Verify([]ed25519.PublicKey{public}, sig, Version, "a.com", "n", "d", nil) {
			t.Errorf("malformed signature %q verified", sig)
		}
	}
}

func TestPublicKeyRoundTrip(t *testing.T) {
	public, _ := testKey(t)
	decoded, err := DecodePublicKey(EncodePublicKey(public))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.Equal(public) {
		t.Error("a key did not survive the round trip")
	}
}

func TestDecodePublicKeyRejectsWrongSizes(t *testing.T) {
	// 31 bytes rather than 32, which RFC 4.2 says must be rejected.
	if _, err := DecodePublicKey("ed25519:" + strings.Repeat("A", 40)); err == nil {
		t.Error("a key of the wrong length was accepted")
	}
	if _, err := DecodePublicKey("MTIz"); err == nil {
		t.Error("a key with no scheme prefix was accepted")
	}
}

func TestNonceIsTwentyTwoBase64URLCharacters(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		nonce, err := NewNonce()
		if err != nil {
			t.Fatalf("NewNonce: %v", err)
		}
		if len(nonce) != 22 {
			t.Fatalf("nonce %q is %d characters, want 22", nonce, len(nonce))
		}
		if strings.ContainsAny(nonce, "+/=") {
			t.Fatalf("nonce %q is not base64url without padding", nonce)
		}
		if seen[nonce] {
			t.Fatalf("nonce %q repeated", nonce)
		}
		seen[nonce] = true
	}
}

// The Date header has to say GMT, not UTC: time.RFC1123 renders the latter and
// some peers parse strictly.
func TestHTTPDateUsesGMT(t *testing.T) {
	date := HTTPDate(time.Date(2026, 4, 23, 10, 15, 0, 0, time.UTC))
	if date != "Thu, 23 Apr 2026 10:15:00 GMT" {
		t.Errorf("HTTPDate = %q", date)
	}
	parsed, err := ParseHTTPDate(date)
	if err != nil {
		t.Fatalf("ParseHTTPDate: %v", err)
	}
	if !parsed.Equal(time.Date(2026, 4, 23, 10, 15, 0, 0, time.UTC)) {
		t.Errorf("round trip = %v", parsed)
	}
}

func TestParseHTTPDateRejectsRubbish(t *testing.T) {
	for _, v := range []string{"", "yesterday", "2026-04-23T10:15:00Z"} {
		if _, err := ParseHTTPDate(v); err == nil {
			t.Errorf("ParseHTTPDate(%q) accepted", v)
		}
	}
}

// writeKey stores a new Ed25519 key as PKCS#8 PEM, the way openssl writes it.
func writeKey(t *testing.T, path string) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

// A Domain that rotated signs with its new key and publishes the old one as a
// previous key for 48 hours, so peers that pinned it keep trusting us, and
// not after (RFC 9.5).
func TestRotatedKeysArePublishedForTheWindow(t *testing.T) {
	dir := t.TempDir()
	current := writeKey(t, filepath.Join(dir, "new.pem"))
	old := writeKey(t, filepath.Join(dir, "old.pem"))
	retired := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	config := filepath.Join(dir, "federation.json")
	if err := os.WriteFile(config, []byte(`{"domains":[{"domain":"example.test",`+
		`"privateKeyFile":"`+filepath.Join(dir, "new.pem")+`",`+
		`"previousKeys":[{"file":"`+filepath.Join(dir, "old.pem")+`","retiredAt":"`+
		retired.Format(time.RFC3339)+`"}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEBMAIL_FEDERATION_CONFIG", config)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t)
	now := retired.Add(47 * time.Hour)
	db.Clock = func() time.Time { return now }
	s, err := New(db, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := s.DescriptorFor("example.test")
	if !ok {
		t.Fatal("no descriptor for example.test")
	}
	if d.PublicKey != EncodePublicKey(current.Public().(ed25519.PublicKey)) {
		t.Error("the descriptor does not carry the new key")
	}
	want := []string{EncodePublicKey(old.Public().(ed25519.PublicKey))}
	if !slices.Equal(d.PreviousPublicKeys, want) {
		t.Errorf("47 hours on, previous_public_keys = %v, want the old key", d.PreviousPublicKeys)
	}
	if err := d.Validate("example.test"); err != nil {
		t.Errorf("the descriptor does not validate: %v", err)
	}

	now = retired.Add(RotationWindow)
	if d, _ := s.DescriptorFor("example.test"); len(d.PreviousPublicKeys) != 0 {
		t.Errorf("48 hours on, previous_public_keys = %v, want none", d.PreviousPublicKeys)
	}
}

// A previous key without the time it was retired is a config error: there
// would be no telling when it has to stop being published.
func TestPreviousKeysNeedARetiredTime(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, filepath.Join(dir, "new.pem"))
	writeKey(t, filepath.Join(dir, "old.pem"))
	config := filepath.Join(dir, "federation.json")
	if err := os.WriteFile(config, []byte(`{"domains":[{"domain":"example.test",`+
		`"privateKeyFile":"`+filepath.Join(dir, "new.pem")+`",`+
		`"previousKeys":[{"file":"`+filepath.Join(dir, "old.pem")+`"}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEBMAIL_FEDERATION_CONFIG", config)
	if _, err := LoadConfig(); err == nil {
		t.Error("a previous key without retiredAt was accepted")
	}
}
