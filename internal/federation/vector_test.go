package federation

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
)

// The test vector of RFC Appendix B, checked end to end: the key, its
// descriptor form, the exact signing input, and the signature. The expected
// signature was computed independently of this package, with OpenSSL's
// Ed25519 over the same input, and Ed25519 signatures are deterministic, so
// any conforming implementation produces exactly these bytes.
func TestAppendixBVector(t *testing.T) {
	seed := mustHex(t, "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	wantPublic := mustHex(t, "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
	const (
		descriptorKey = "ed25519:11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="
		version       = "0.2"
		sender        = "103mail.com"
		nonce         = "AbCdEfGhIjKlMnOpQrStUw"
		date          = "Thu, 23 Apr 2026 10:15:00 GMT"
		body          = `{"version":"0.2","envelope_id":"x"}`
		wantSignature = "ed25519:yIBEYJueERqYiye1shfinI0NfcnbsyC/UZxRDPcZMnPNZFm3dL6hiy7/jYj9PSUXVq3t1AXLSD/4Z2KEV8mqBQ=="
	)

	key := ed25519.NewKeyFromSeed(seed)
	public := key.Public().(ed25519.PublicKey)
	if !bytes.Equal(public, wantPublic) {
		t.Fatalf("derived public key %x, want %x", public, wantPublic)
	}
	if got := EncodePublicKey(public); got != descriptorKey {
		t.Errorf("descriptor form %q, want %q", got, descriptorKey)
	}
	if decoded, err := DecodePublicKey(descriptorKey); err != nil || !bytes.Equal(decoded, public) {
		t.Errorf("DecodePublicKey(%q) = %x, %v", descriptorKey, decoded, err)
	}

	// The body is the Content-Length the request carries.
	if len(body) != 35 {
		t.Errorf("the body is %d bytes, want 35", len(body))
	}
	wantInput := strings.Join([]string{version, sender, nonce, date, body}, "\n")
	input := signingInput(version, sender, nonce, date, []byte(body))
	if string(input) != wantInput || len(input) != 104 {
		t.Errorf("signing input %q (%d bytes), want %q", input, len(input), wantInput)
	}

	signature := Sign(key, version, sender, nonce, date, []byte(body))
	if signature != wantSignature {
		t.Errorf("signature %s, want %s", signature, wantSignature)
	}
	if !Verify([]ed25519.PublicKey{public}, signature, version, sender, nonce, date, []byte(body)) {
		t.Error("the vector's signature does not verify")
	}
	// Any change to what is signed must fail, the body included.
	if Verify([]ed25519.PublicKey{public}, signature, version, sender, nonce, date, []byte(body+" ")) {
		t.Error("a signature verified over a different body")
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
