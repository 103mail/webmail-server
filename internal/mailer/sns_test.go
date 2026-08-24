package mailer

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// snsHarness is a stand-in for AWS: a signing key, the certificate that goes
// with it, and a server that hands the certificate out.
type snsHarness struct {
	key      *rsa.PrivateKey
	server   *httptest.Server
	certURL  string
	confirms int
}

func newSNSHarness(t *testing.T) *snsHarness {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "sns.eu-west-1.amazonaws.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	harness := &snsHarness{key: key}
	harness.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cert.pem":
			_, _ = w.Write(certPEM)
		case "/confirm":
			harness.confirms++
			_, _ = w.Write([]byte("<ConfirmSubscriptionResponse/>"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(harness.server.Close)
	harness.certURL = harness.server.URL + "/cert.pem"
	return harness
}

// verifier returns a Verifier that trusts the harness server in place of AWS.
func (h *snsHarness) verifier(topicARNs ...string) *Verifier {
	v := NewVerifier(h.server.Client(), topicARNs...)
	v.trustHost = func(*url.URL) error { return nil }
	return v
}

func (h *snsHarness) notification(t *testing.T, message string) *Notification {
	t.Helper()
	n := &Notification{
		Type:           TypeNotification,
		MessageID:      "11111111-2222-3333-4444-555555555555",
		TopicArn:       "arn:aws:sns:eu-west-1:123456789012:webmail-inbound",
		Message:        message,
		Timestamp:      "2026-08-24T10:30:00.000Z",
		SigningCertURL: h.certURL,
	}
	h.sign(t, n, "2")
	return n
}

func (h *snsHarness) sign(t *testing.T, n *Notification, version string) {
	t.Helper()
	canonical, err := canonicalMessage(n)
	if err != nil {
		t.Fatalf("canonicalMessage: %v", err)
	}
	var hash crypto.Hash
	var digest []byte
	if version == "1" {
		sum := sha1.Sum([]byte(canonical))
		hash, digest = crypto.SHA1, sum[:]
	} else {
		sum := sha256.Sum256([]byte(canonical))
		hash, digest = crypto.SHA256, sum[:]
	}
	signature, err := rsa.SignPKCS1v15(rand.Reader, h.key, hash, digest)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	n.SignatureVersion = version
	n.Signature = base64.StdEncoding.EncodeToString(signature)
}

// Given an SNS post body, when it is parsed, then the envelope fields come out
// unauthenticated but intact.
func TestParseNotification(t *testing.T) {
	body := []byte(`{"Type":"Notification","MessageId":"abc","TopicArn":"arn:aws:sns:eu-west-1:1:t","Message":"hello","Timestamp":"2026-08-24T10:30:00.000Z"}`)

	n, err := ParseNotification(body)
	if err != nil {
		t.Fatalf("ParseNotification() error = %v", err)
	}
	if n.Type != TypeNotification || n.MessageID != "abc" || n.Message != "hello" {
		t.Fatalf("ParseNotification() = %+v, want the envelope fields", n)
	}
}

// Given a body that is not an SNS envelope, when it is parsed, then it is
// rejected.
func TestParseNotificationRejectsNonEnvelope(t *testing.T) {
	for _, body := range []string{`not json`, `{}`, `{"MessageId":"abc"}`} {
		if _, err := ParseNotification([]byte(body)); err == nil {
			t.Errorf("ParseNotification(%q) = nil error, want a failure", body)
		}
	}
}

// Given a notification genuinely signed by AWS, when it is verified, then it
// is accepted, whichever signature version the topic uses.
func TestVerifierAcceptsSignedNotification(t *testing.T) {
	harness := newSNSHarness(t)
	for _, version := range []string{"1", "2"} {
		t.Run("version "+version, func(t *testing.T) {
			n := harness.notification(t, "hello")
			harness.sign(t, n, version)
			if err := harness.verifier().Verify(context.Background(), n); err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
		})
	}
}

// Given a notification whose contents were changed after signing, when it is
// verified, then it is rejected: this is what stops anyone who can reach the
// endpoint from injecting mail.
func TestVerifierRejectsTamperedNotification(t *testing.T) {
	harness := newSNSHarness(t)
	tests := []struct {
		name   string
		mutate func(*Notification)
	}{
		{"message body", func(n *Notification) { n.Message = "forged" }},
		{"topic", func(n *Notification) { n.TopicArn = "arn:aws:sns:eu-west-1:123456789012:other" }},
		{"timestamp", func(n *Notification) { n.Timestamp = "2020-01-01T00:00:00.000Z" }},
		{"signature", func(n *Notification) { n.Signature = base64.StdEncoding.EncodeToString([]byte("nope")) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			n := harness.notification(t, "hello")
			test.mutate(n)
			if err := harness.verifier().Verify(context.Background(), n); err == nil {
				t.Fatal("Verify() = nil, want a signature failure")
			}
		})
	}
}

// Given a notification from a topic we do not subscribe to, when it is
// verified, then it is rejected even though its signature is valid.
func TestVerifierRejectsUnknownTopic(t *testing.T) {
	harness := newSNSHarness(t)
	n := harness.notification(t, "hello")

	verifier := harness.verifier("arn:aws:sns:eu-west-1:123456789012:some-other-topic")
	err := verifier.Verify(context.Background(), n)
	if err == nil || !strings.Contains(err.Error(), "not accepted") {
		t.Fatalf("Verify() = %v, want the topic to be refused", err)
	}
}

// Given a signature version SNS does not define, when a notification is
// verified, then it is rejected rather than trusted.
func TestVerifierRejectsUnknownSignatureVersion(t *testing.T) {
	harness := newSNSHarness(t)
	n := harness.notification(t, "hello")
	n.SignatureVersion = "3"

	if err := harness.verifier().Verify(context.Background(), n); err == nil {
		t.Fatal("Verify() = nil, want an unsupported version error")
	}
}

// Given a notification pointing its certificate somewhere other than AWS, when
// it is verified, then the certificate is never fetched: a forged post must
// not be able to choose the key that validates it.
func TestVerifierRejectsForeignCertificateURL(t *testing.T) {
	harness := newSNSHarness(t)
	tests := []string{
		"http://sns.eu-west-1.amazonaws.com/cert.pem",
		"https://evil.example.com/cert.pem",
		"https://sns.eu-west-1.amazonaws.com.evil.example.com/cert.pem",
		"https://sns.eu-west-1.amazonaws.com@evil.example.com/cert.pem",
		"https://s3.eu-west-1.amazonaws.com/cert.pem",
	}
	for _, certURL := range tests {
		t.Run(certURL, func(t *testing.T) {
			n := harness.notification(t, "hello")
			n.SigningCertURL = certURL
			// A real Verifier, with the AWS host check in place.
			verifier := NewVerifier(harness.server.Client(), n.TopicArn)
			if err := verifier.Verify(context.Background(), n); err == nil {
				t.Fatalf("Verify() = nil, want %q to be refused", certURL)
			}
		})
	}
}

// Given an AWS certificate URL, when it is checked, then it is allowed.
func TestVerifierAcceptsAWSCertificateURL(t *testing.T) {
	verifier := NewVerifier(nil)
	for _, certURL := range []string{
		"https://sns.eu-west-1.amazonaws.com/SimpleNotificationService-1234.pem",
		"https://sns.cn-north-1.amazonaws.com.cn/SimpleNotificationService-1234.pem",
	} {
		if _, err := verifier.checkURL(certURL); err != nil {
			t.Errorf("checkURL(%q) = %v, want nil", certURL, err)
		}
	}
}

// Given the same certificate needed twice, when two notifications are
// verified, then it is fetched once: SNS rotates keys rarely and every
// message would otherwise cost a round trip.
func TestVerifierCachesCertificate(t *testing.T) {
	harness := newSNSHarness(t)
	fetches := 0
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		harness.server.Config.Handler.ServeHTTP(w, r)
	}))
	defer counting.Close()

	verifier := harness.verifier()
	verifier.client = counting.Client()
	for i := 0; i < 3; i++ {
		n := harness.notification(t, "hello")
		n.SigningCertURL = counting.URL + "/cert.pem"
		harness.sign(t, n, "2")
		if err := verifier.Verify(context.Background(), n); err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
	}
	if fetches != 1 {
		t.Fatalf("fetched the certificate %d times, want 1", fetches)
	}
}

// Given a subscription confirmation from AWS, when it is confirmed, then the
// SubscribeURL is fetched, which is what completes the subscription.
func TestVerifierConfirmFetchesSubscribeURL(t *testing.T) {
	harness := newSNSHarness(t)
	n := &Notification{
		Type:           TypeSubscriptionConfirmation,
		MessageID:      "sub-1",
		TopicArn:       "arn:aws:sns:eu-west-1:123456789012:webmail-inbound",
		Message:        "You have chosen to subscribe",
		Token:          "token-value",
		SubscribeURL:   harness.server.URL + "/confirm",
		Timestamp:      "2026-08-24T10:30:00.000Z",
		SigningCertURL: harness.certURL,
	}
	harness.sign(t, n, "2")

	if err := harness.verifier(n.TopicArn).Confirm(context.Background(), n); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if harness.confirms != 1 {
		t.Fatalf("SubscribeURL fetched %d times, want 1", harness.confirms)
	}
}

// Given a confirmation whose SubscribeURL points away from AWS, when it is
// confirmed, then nothing is fetched: the endpoint must not become a way to
// make this server call arbitrary addresses.
func TestVerifierConfirmRejectsForeignSubscribeURL(t *testing.T) {
	harness := newSNSHarness(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Confirm() fetched a non-AWS SubscribeURL")
	}))
	defer target.Close()

	n := &Notification{
		Type:           TypeSubscriptionConfirmation,
		MessageID:      "sub-1",
		TopicArn:       "arn:aws:sns:eu-west-1:123456789012:webmail-inbound",
		Message:        "You have chosen to subscribe",
		Token:          "token-value",
		SubscribeURL:   target.URL + "/confirm",
		Timestamp:      "2026-08-24T10:30:00.000Z",
		SigningCertURL: harness.certURL,
	}
	harness.sign(t, n, "2")

	// The certificate host check has to stay off so that the SubscribeURL is
	// the only thing under test, and it is checked separately.
	verifier := harness.verifier(n.TopicArn)
	verifier.trustHost = func(u *url.URL) error {
		if strings.HasSuffix(u.Path, "/cert.pem") {
			return nil
		}
		return errors.New("not an aws host")
	}
	if err := verifier.Confirm(context.Background(), n); err == nil {
		t.Fatal("Confirm() = nil, want the foreign SubscribeURL to be refused")
	}
}

// Given an ordinary notification, when Confirm is called on it, then it is
// refused: only a subscription message is a subscription.
func TestVerifierConfirmRejectsPlainNotification(t *testing.T) {
	harness := newSNSHarness(t)
	n := harness.notification(t, "hello")
	if err := harness.verifier().Confirm(context.Background(), n); err == nil {
		t.Fatal("Confirm() = nil, want a type error")
	}
}
