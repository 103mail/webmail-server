package mailer

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Notification is the Amazon SNS envelope an inbound message arrives in. SES
// publishes to a topic; SNS posts this JSON to our endpoint.
type Notification struct {
	Type             string `json:"Type"`
	MessageID        string `json:"MessageId"`
	TopicArn         string `json:"TopicArn"`
	Subject          string `json:"Subject"`
	Message          string `json:"Message"`
	Timestamp        string `json:"Timestamp"`
	SignatureVersion string `json:"SignatureVersion"`
	Signature        string `json:"Signature"`
	SigningCertURL   string `json:"SigningCertURL"`
	SubscribeURL     string `json:"SubscribeURL"`
	UnsubscribeURL   string `json:"UnsubscribeURL"`
	Token            string `json:"Token"`
}

// The SNS message types we handle.
const (
	TypeNotification             = "Notification"
	TypeSubscriptionConfirmation = "SubscriptionConfirmation"
	TypeUnsubscribeConfirmation  = "UnsubscribeConfirmation"
)

const (
	maxCertBytes  = 64 << 10
	maxCachedCert = 32
)

// ParseNotification reads an SNS POST body. It does not authenticate it:
// pass the result to Verifier.Verify before believing anything in it.
func ParseNotification(body []byte) (*Notification, error) {
	var n Notification
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, fmt.Errorf("parse sns notification: %w", err)
	}
	if n.Type == "" {
		return nil, errors.New("mailer: sns notification has no type")
	}
	return &n, nil
}

// ErrNotVerified reports an SNS post this server does not believe: a bad
// signature, a certificate that is not Amazon's, or a topic we do not accept.
// It is distinct from a processing failure because a post we do not believe is
// answered with 403 and never retried, while a failure of ours is a 5xx that
// SNS should try again.
var ErrNotVerified = errors.New("mailer: sns notification is not verified")

// Verifier authenticates SNS messages. Without it any host that can reach the
// inbound endpoint could post mail into someone's mailbox.
//
// It does not detect replays. SNS itself delivers at least once, so the domain
// layer has to be idempotent on Inbound.ProviderID regardless, and that same
// check covers a replayed message.
type Verifier struct {
	client *http.Client
	topics map[string]bool

	// trustHost overrides the check that a certificate or subscription URL
	// belongs to AWS. It is nil outside this package's own tests.
	trustHost func(*url.URL) error

	mu    sync.Mutex
	certs map[string]*x509.Certificate
}

// NewVerifier returns a Verifier that accepts messages from the given topic
// ARNs. Passing no ARN accepts any topic, which is only appropriate when the
// endpoint is not reachable from the internet.
//
// A nil client gets one with a short timeout, since the only thing it fetches
// is a signing certificate.
func NewVerifier(client *http.Client, topicARNs ...string) *Verifier {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	topics := make(map[string]bool, len(topicARNs))
	for _, arn := range topicARNs {
		topics[arn] = true
	}
	return &Verifier{
		client: client,
		topics: topics,
		certs:  map[string]*x509.Certificate{},
	}
}

// Verify checks that n came from AWS and from a topic we accept. Every failure
// wraps ErrNotVerified, so a caller can tell "we do not believe this post"
// apart from "we could not process it".
func (v *Verifier) Verify(ctx context.Context, n *Notification) error {
	if len(v.topics) > 0 && !v.topics[n.TopicArn] {
		return fmt.Errorf("%w: sns topic %q is not accepted", ErrNotVerified, n.TopicArn)
	}
	canonical, err := canonicalMessage(n)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotVerified, err)
	}
	certURL, err := v.checkURL(n.SigningCertURL)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotVerified, err)
	}
	cert, err := v.certificate(ctx, certURL.String())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotVerified, err)
	}
	if err := verifySignature(cert, n.SignatureVersion, canonical, n.Signature); err != nil {
		return fmt.Errorf("%w: %w", ErrNotVerified, err)
	}
	return nil
}

// Confirm completes an SNS subscription by fetching the SubscribeURL. It
// verifies n first and checks that the URL points at AWS, so a forged post
// cannot turn this server into a fetcher for an arbitrary address.
func (v *Verifier) Confirm(ctx context.Context, n *Notification) error {
	if n.Type != TypeSubscriptionConfirmation && n.Type != TypeUnsubscribeConfirmation {
		return fmt.Errorf("mailer: sns message of type %q is not a subscription confirmation", n.Type)
	}
	if err := v.Verify(ctx, n); err != nil {
		return err
	}
	subscribeURL, err := v.checkURL(n.SubscribeURL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, subscribeURL.String(), nil)
	if err != nil {
		return fmt.Errorf("confirm sns subscription: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("confirm sns subscription: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxCertBytes))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("confirm sns subscription: status %d", resp.StatusCode)
	}
	return nil
}

// canonicalMessage rebuilds the string AWS signed: each signed field as a name
// line and a value line, in the order SNS documents.
func canonicalMessage(n *Notification) (string, error) {
	var fields [][2]string
	switch n.Type {
	case TypeNotification:
		fields = [][2]string{{"Message", n.Message}, {"MessageId", n.MessageID}}
		if n.Subject != "" {
			fields = append(fields, [2]string{"Subject", n.Subject})
		}
		fields = append(fields,
			[2]string{"Timestamp", n.Timestamp},
			[2]string{"TopicArn", n.TopicArn},
			[2]string{"Type", n.Type})
	case TypeSubscriptionConfirmation, TypeUnsubscribeConfirmation:
		fields = [][2]string{
			{"Message", n.Message},
			{"MessageId", n.MessageID},
			{"SubscribeURL", n.SubscribeURL},
			{"Timestamp", n.Timestamp},
			{"Token", n.Token},
			{"TopicArn", n.TopicArn},
			{"Type", n.Type},
		}
	default:
		return "", fmt.Errorf("mailer: unknown sns message type %q", n.Type)
	}

	var b strings.Builder
	for _, field := range fields {
		b.WriteString(field[0])
		b.WriteByte('\n')
		b.WriteString(field[1])
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func verifySignature(cert *x509.Certificate, version, canonical, signature string) error {
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("mailer: sns signature is not base64: %w", err)
	}
	key, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return errors.New("mailer: sns signing certificate does not hold an RSA key")
	}

	var hash crypto.Hash
	var digest []byte
	switch version {
	case "1":
		// SHA-1 is weak, but signature version 1 is defined to use it and
		// some SNS topics still emit it. Version 2 is the one to configure.
		sum := sha1.Sum([]byte(canonical))
		hash, digest = crypto.SHA1, sum[:]
	case "2":
		sum := sha256.Sum256([]byte(canonical))
		hash, digest = crypto.SHA256, sum[:]
	default:
		return fmt.Errorf("mailer: unsupported sns signature version %q", version)
	}
	if err := rsa.VerifyPKCS1v15(key, hash, digest, sig); err != nil {
		return fmt.Errorf("mailer: sns signature does not verify: %w", err)
	}
	return nil
}

// checkURL rejects anything that is not an https SNS endpoint at Amazon.
func (v *Verifier) checkURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("mailer: parse sns url %q: %w", raw, err)
	}
	if v.trustHost != nil {
		return parsed, v.trustHost(parsed)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("mailer: sns url %q is not https", raw)
	}
	host := strings.ToLower(parsed.Hostname())
	if !strings.HasPrefix(host, "sns.") {
		return nil, fmt.Errorf("mailer: sns url %q is not an sns host", raw)
	}
	if !strings.HasSuffix(host, ".amazonaws.com") && !strings.HasSuffix(host, ".amazonaws.com.cn") {
		return nil, fmt.Errorf("mailer: sns url %q is not an amazon host", raw)
	}
	return parsed, nil
}

// certificate fetches and caches an SNS signing certificate. AWS rotates them
// rarely, so caching turns a network round trip per message into one per
// rotation.
func (v *Verifier) certificate(ctx context.Context, certURL string) (*x509.Certificate, error) {
	v.mu.Lock()
	cached, ok := v.certs[certURL]
	v.mu.Unlock()
	if ok {
		return cached, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, certURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch sns certificate: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch sns certificate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch sns certificate: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCertBytes))
	if err != nil {
		return nil, fmt.Errorf("read sns certificate: %w", err)
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, errors.New("mailer: sns certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse sns certificate: %w", err)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	// The URL is already known to be an AWS one, so the cache cannot be
	// filled with attacker chosen keys. The cap is only there to stop it
	// growing without bound across regions and rotations.
	if len(v.certs) >= maxCachedCert {
		v.certs = map[string]*x509.Certificate{}
	}
	v.certs[certURL] = cert
	return cert, nil
}
