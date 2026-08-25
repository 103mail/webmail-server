package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"
)

// keyPrefix is the encoding every key on the wire carries. Ed25519 is the only
// scheme in this version, and the prefix is what lets a later version add one
// without ambiguity.
const keyPrefix = "ed25519:"

// signingInput builds the bytes covered by a signature (RFC 6.2):
//
//	version \n sender \n nonce \n date \n body
//
// Note what is and is not in here. The four headers are, so a captured request
// cannot be replayed with a fresh Date. The URL is not, because the path is
// fixed by the RFC and the host is authenticated by TLS. There is no
// pre-hashing; Ed25519 hashes internally.
func signingInput(version, sender, nonce, date string, body []byte) []byte {
	input := make([]byte, 0, len(version)+len(sender)+len(nonce)+len(date)+len(body)+4)
	for _, header := range []string{version, sender, nonce, date} {
		input = append(input, header...)
		input = append(input, '\n')
	}
	return append(input, body...)
}

// Sign returns the value of X-Federation-Signature for a request.
func Sign(key ed25519.PrivateKey, version, sender, nonce, date string, body []byte) string {
	sig := ed25519.Sign(key, signingInput(version, sender, nonce, date, body))
	return keyPrefix + base64.StdEncoding.EncodeToString(sig)
}

// Verify reports whether the signature matches any of the sender's currently
// valid keys. The whole keyset is tried because a peer mid-rotation signs with
// its new key while some peers still have only the old one cached (RFC 9.5).
func Verify(keys []ed25519.PublicKey, signature, version, sender, nonce, date string, body []byte) bool {
	raw, ok := decodeSignature(signature)
	if !ok {
		return false
	}
	input := signingInput(version, sender, nonce, date, body)
	for _, key := range keys {
		if len(key) == ed25519.PublicKeySize && ed25519.Verify(key, input, raw) {
			return true
		}
	}
	return false
}

func decodeSignature(v string) ([]byte, bool) {
	if !strings.HasPrefix(v, keyPrefix) {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, keyPrefix))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, false
	}
	return raw, true
}

// EncodePublicKey renders a key the way a descriptor carries it.
func EncodePublicKey(key ed25519.PublicKey) string {
	return keyPrefix + base64.StdEncoding.EncodeToString(key)
}

// DecodePublicKey parses `ed25519:{base64}`. A key that does not decode to
// exactly 32 bytes is a reason to reject the whole descriptor (RFC 4.2).
func DecodePublicKey(v string) (ed25519.PublicKey, error) {
	if !strings.HasPrefix(v, keyPrefix) {
		return nil, fmt.Errorf("public key %q does not start with %s", v, keyPrefix)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, keyPrefix))
	if err != nil {
		return nil, fmt.Errorf("decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// LoadPrivateKey reads a PKCS#8 Ed25519 key in PEM, which is what
// `openssl genpkey -algorithm ed25519` writes.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s is not PEM", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s holds a %T, want an Ed25519 private key", path, parsed)
	}
	return key, nil
}

// NewNonce returns a 128-bit value as unpadded base64url, the encoding RFC 6.1
// requires. Twenty-two characters.
func NewNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// imfFixdate is RFC 7231's preferred Date format. time.RFC1123 is the same
// layout but renders the zone from the value, which yields "UTC" rather than
// the "GMT" the spec asks for.
const imfFixdate = "Mon, 02 Jan 2006 15:04:05 GMT"

// HTTPDate renders the Date header in the IMF-fixdate form RFC 7231 wants.
func HTTPDate(t time.Time) string {
	return t.UTC().Format(imfFixdate)
}

// ParseHTTPDate accepts the three date formats HTTP has accumulated, because a
// peer may legitimately send any of them.
func ParseHTTPDate(v string) (time.Time, error) {
	for _, layout := range []string{imfFixdate, time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable Date header %q", v)
}
