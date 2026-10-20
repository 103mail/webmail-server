// Package federation implements the Webmail-Federation protocol: peer
// discovery, envelope transfer between Domains, and the outbox that carries
// outbound envelopes across an unreliable network. See ADR 17, and
// `../../../webmail-federation/project_rfc.md`, which is normative.
//
// The package owns the wire and nothing else. It never imports the mail domain;
// inbound envelopes are handed to a Deliverer, and outbound envelopes are
// enqueued by the mail domain through an interface it declares itself. That is
// what keeps the dependency in one direction and the two testable apart.
package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mevdschee/webmail-server/internal/store"
)

// Timing rules, all from the RFC. They are constants rather than settings
// because a peer that disagrees about them is a peer that fails to interoperate.
const (
	// ClockSkew is how far a peer's Date may be from ours (RFC 6.2).
	ClockSkew = 5 * time.Minute
	// NonceWindow is how long a nonce is remembered per sender (RFC 9.4).
	NonceWindow = 10 * time.Minute
	// DedupRetention keeps accepted envelope ids well past the retry budget
	// so a late redelivery still replays rather than duplicates (RFC 5.4).
	DedupRetention = 96 * time.Hour
	// RefreshInterval is the most often a cached descriptor is re-fetched,
	// and FailureBackoff the least often a failed peer is re-probed (RFC 4.3).
	RefreshInterval = 24 * time.Hour
	FailureBackoff  = time.Hour

	// Retry schedule for transient failures (RFC 6.4).
	RetryBase   = 30 * time.Second
	RetryCap    = time.Hour
	RetryBudget = 72 * time.Hour

	// Lease is how long a worker holds the outbox rows it claimed. It is ours,
	// not the RFC's, and only has to outlast one delivery pass: a worker that
	// dies holding rows delays them by at most this much.
	Lease = 10 * time.Minute
)

// LocalDomain is a Domain this Server is authoritative for.
type LocalDomain struct {
	// Domain is the DNS domain. The Federation hostname is derived from it and
	// is never configured, so there is nothing here to point elsewhere.
	Domain string `json:"domain"`
	// CustomerID is the tenant that owns the Domain. Internal: the protocol
	// never transmits it and peers must not be able to infer it (RFC 9.3).
	CustomerID int64 `json:"customerId"`
	// PrivateKeyFile is a PEM PKCS#8 Ed25519 key. Each Domain has its own,
	// because peers pin keys per DNS domain and one Domain's compromise must
	// not let an attacker speak for the others co-hosted with it (RFC 4.1).
	PrivateKeyFile string `json:"privateKeyFile"`

	key ed25519.PrivateKey
}

// Config is the federation configuration, read from a JSON file so that a
// per-Domain keypair has somewhere to live. Absent, federation is off and the
// server behaves exactly as it did before: every recipient is local.
type Config struct {
	Domains []LocalDomain `json:"domains"`
	// AllowedPeers, when non-empty, is the only set of Domains envelopes are
	// accepted from. Empty accepts any Domain that discovery validates, which
	// is the open-federation default (RFC 9.7).
	AllowedPeers []string `json:"allowedPeers"`
	// Caps this Domain advertises and enforces on what it receives.
	MaxEnvelopeBytes   int64 `json:"maxEnvelopeBytes"`
	MaxAttachmentBytes int64 `json:"maxAttachmentBytes"`
	// ExtraCAFile adds a certificate authority to the pool used for peer
	// connections. It exists for a local or staging deployment whose peers hold
	// certificates from a private CA; adding a CA to our own trust store is not
	// the same as skipping verification, which this package never does.
	ExtraCAFile string `json:"extraCaFile"`
}

// LoadConfig reads the file named by WEBMAIL_FEDERATION_CONFIG. An unset
// variable returns a nil Config, which disables federation.
func LoadConfig() (*Config, error) {
	path := os.Getenv("WEBMAIL_FEDERATION_CONFIG")
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read federation config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse federation config %s: %w", path, err)
	}
	if cfg.MaxEnvelopeBytes <= 0 {
		cfg.MaxEnvelopeBytes = DefaultMaxEnvelopeBytes
	}
	if cfg.MaxAttachmentBytes <= 0 {
		cfg.MaxAttachmentBytes = DefaultMaxAttachmentBytes
	}
	for i := range cfg.Domains {
		d := &cfg.Domains[i]
		if err := checkDomainName(d.Domain); err != nil {
			return nil, err
		}
		d.Domain = strings.ToLower(d.Domain)
		if d.PrivateKeyFile == "" {
			return nil, fmt.Errorf("domain %s has no privateKeyFile", d.Domain)
		}
		key, err := LoadPrivateKey(d.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("domain %s: %w", d.Domain, err)
		}
		d.key = key
	}
	return &cfg, nil
}

// checkDomainName refuses at load time the configurations RFC 9.2 forbids: a
// scheme, a port, an IP address, or a hostname that is not a bare DNS domain.
// The Federation hostname is derived, so a config that names one is a config
// that has already drifted from the specification.
func checkDomainName(name string) error {
	if name == "" {
		return fmt.Errorf("federation config has a domain with no name")
	}
	if strings.Contains(name, "://") || strings.Contains(name, "/") {
		return fmt.Errorf("domain %q must be a bare DNS name, not a URL", name)
	}
	if strings.Contains(name, ":") {
		return fmt.Errorf("domain %q must not carry a port", name)
	}
	if net.ParseIP(name) != nil {
		return fmt.Errorf("domain %q is an IP address, which cannot hold a Federation hostname", name)
	}
	if !strings.Contains(name, ".") {
		return fmt.Errorf("domain %q is not a DNS domain", name)
	}
	if strings.HasPrefix(name, "webmail.") {
		return fmt.Errorf("domain %q looks like a Federation hostname; configure the DNS domain instead", name)
	}
	return nil
}

// Deliverer writes an accepted envelope into the mail domain. Declared here and
// satisfied by *email.Service, so this package never imports that one.
type Deliverer interface {
	// DeliverInbound resolves each recipient to a local account and writes the
	// canonical rows plus one projection per recipient. It returns how many
	// recipients it delivered to, and the local recipients it did not deliver
	// to. None delivered is answered 404, some refused is listed in the 202
	// (RFC 6.3, 6.5).
	DeliverInbound(ctx context.Context, in Inbound) (int, []string, error)
	// Undelivered lists the local recipients of an already accepted envelope
	// that have no copy of its message. It only reads, which is what lets a
	// replay be answered with the original list (RFC 5.4).
	Undelivered(ctx context.Context, in Inbound) ([]string, error)
	// Bounce writes a delivery failure into the original sender's own mailbox.
	// It is local and never leaves this Server (RFC 6.5).
	Bounce(ctx context.Context, b BounceNotice) error
}

// Inbound is an accepted envelope, decoded and checked, ready to be written.
type Inbound struct {
	OriginDomain string
	ThreadFedID  string
	InReplyTo    string
	Subject      string
	MessageFedID string
	SenderEmail  string
	SenderName   string
	Body         string
	BodyHTML     string
	SentAt       time.Time
	Recipients   []string
	// Members is everyone on the thread, on every Domain (RFC 5.3).
	Members []string
	// Refused and Left are addresses on the sender's own domain that are no
	// longer members: refused as unknown, or departed (RFC 5.3, 5.6).
	Refused       []string
	Left          []string
	Attachments   []InboundAttachment
	DestinationID int64 // customer that owns the receiving Domain, 0 if unknown
}

// InboundAttachment is one attachment with its bytes already decoded and
// verified against the declared size and hash.
type InboundAttachment struct {
	FederationID string
	FileName     string
	MimeType     string
	Data         []byte
}

// BounceNotice is a permanently failed delivery, reported back to the sender.
type BounceNotice struct {
	OriginDomain string
	SenderEmail  string
	Subject      string
	// ThreadFedID is the thread the failed message was on.
	ThreadFedID string
	// Refused means the recipients do not exist at the peer, so they are not
	// members of the thread and later messages should not go to them. A peer
	// that was unreachable says nothing of the kind (RFC 6.5).
	Refused    bool
	Recipients []string
	PeerDomain string
	EnvelopeID string
	HTTPStatus int
	ErrorCode  string
}

// Service is the federation runtime: the local Domain registry, the peer cache
// and the transport.
type Service struct {
	db  *store.DB
	cfg *Config
	log *slog.Logger

	local map[string]*LocalDomain

	// Two clients with different budgets. Discovery is a small idempotent GET
	// that should fail fast; delivery carries attachments and needs longer.
	discoveryClient *http.Client
	deliveryClient  *http.Client

	deliverer Deliverer

	// worker names this process in the outbox leases it takes, so a row shows
	// which worker holds it.
	worker string
}

// New returns a Service. cfg may be nil, which yields a Service that reports
// every domain as local, so callers do not have to nil-check it.
func New(db *store.DB, cfg *Config, log *slog.Logger, deliverer Deliverer) (*Service, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	worker, err := workerName()
	if err != nil {
		return nil, err
	}
	s := &Service{db: db, cfg: cfg, log: log, deliverer: deliverer, local: map[string]*LocalDomain{},
		worker: worker}
	if cfg == nil {
		return s, nil
	}
	for i := range cfg.Domains {
		s.local[cfg.Domains[i].Domain] = &cfg.Domains[i]
	}

	tlsConfig, err := peerTLSConfig(cfg.ExtraCAFile)
	if err != nil {
		return nil, err
	}
	// No redirects on either client: RFC 4.1 and 6.1 both say a 3xx aborts.
	noRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	transport := &http.Transport{
		TLSClientConfig:     tlsConfig,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConnsPerHost: 4,
	}
	s.discoveryClient = &http.Client{
		Transport: transport, CheckRedirect: noRedirect, Timeout: 10 * time.Second,
	}
	s.deliveryClient = &http.Client{
		Transport: transport, CheckRedirect: noRedirect, Timeout: 30 * time.Second,
	}
	return s, nil
}

// workerName is the host name and a random suffix, short enough for the
// claimed_by column. The suffix tells apart two workers on one host.
func workerName() (string, error) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	suffix, err := NewNonce()
	if err != nil {
		return "", err
	}
	name := host + "/" + suffix
	if len(name) > 64 {
		name = name[len(name)-64:]
	}
	return name, nil
}

// peerTLSConfig builds the TLS settings for peer connections. TLS 1.2 is the
// floor and verification is always on; the only thing configurable is adding a
// CA to the pool, never removing the check (RFC 9.2).
func peerTLSConfig(extraCAFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if extraCAFile == "" {
		return cfg, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pem, err := os.ReadFile(extraCAFile)
	if err != nil {
		return nil, fmt.Errorf("read extraCaFile: %w", err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("extraCaFile %s holds no certificate", extraCAFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// Attach supplies the mail domain that receives accepted envelopes and writes
// bounces. It exists because the two services refer to each other: the mail
// domain needs the federation service to enqueue, and the federation service
// needs the mail domain to deliver. One of the two has to be wired after
// construction, and this is the smaller half.
func (s *Service) Attach(d Deliverer) { s.deliverer = d }

// Enabled reports whether any local Domain is configured.
func (s *Service) Enabled() bool { return s != nil && len(s.local) > 0 }

// IsLocal reports whether a DNS domain is one this Server is authoritative for.
// With federation off every domain is local, which is the single-Domain
// behaviour this replaces.
func (s *Service) IsLocal(domain string) bool {
	if !s.Enabled() {
		return true
	}
	_, ok := s.local[strings.ToLower(domain)]
	return ok
}

// LocalDomains returns the configured Domains, for the descriptor endpoint.
func (s *Service) LocalDomains() []string {
	names := make([]string, 0, len(s.local))
	for name := range s.local {
		names = append(names, name)
	}
	return names
}

// DescriptorFor renders the peer descriptor of one local Domain. Each Domain
// gets its own; descriptors are never shared across DNS domains (RFC 4.1).
func (s *Service) DescriptorFor(domain string) (*Descriptor, bool) {
	d, ok := s.local[strings.ToLower(domain)]
	if !ok {
		return nil, false
	}
	return &Descriptor{
		Domain:             d.Domain,
		PublicKey:          EncodePublicKey(d.key.Public().(ed25519.PublicKey)),
		PreviousPublicKeys: []string{},
		Versions:           []string{Version},
		Registrar:          false,
		MaxEnvelopeBytes:   s.cfg.MaxEnvelopeBytes,
		MaxAttachmentBytes: s.cfg.MaxAttachmentBytes,
	}, true
}

// OriginFor picks the local Domain an address sends as. An address on a Domain
// this Server hosts sends as that Domain; anything else has no origin and
// cannot be federated from here.
func (s *Service) OriginFor(senderEmail string) (*LocalDomain, bool) {
	d, ok := s.local[DomainOf(senderEmail)]
	return d, ok
}

// peerAllowed applies the operator allowlist. An empty list is open federation.
func (s *Service) peerAllowed(domain string) bool {
	if s.cfg == nil || len(s.cfg.AllowedPeers) == 0 {
		return true
	}
	for _, allowed := range s.cfg.AllowedPeers {
		if strings.EqualFold(allowed, domain) {
			return true
		}
	}
	return false
}

// Peer returns the cached descriptor for a DNS domain, fetching it if it has
// never been seen or the cache is stale.
//
// The caching rules are RFC 4.3: refresh no more than once a day, re-probe a
// failed peer no more than once an hour, and refresh on demand when a signature
// fails to verify, which is how a key rotation is noticed.
func (s *Service) Peer(ctx context.Context, domain string, force bool) (*Descriptor, error) {
	domain = strings.ToLower(domain)
	cached, discoveredAt, failedAt, err := s.cachedPeer(ctx, domain)
	if err != nil {
		return nil, err
	}
	now := s.db.Now()
	fresh := cached != nil && !discoveredAt.IsZero() && now.Sub(discoveredAt) < RefreshInterval
	if fresh && !force {
		return cached, nil
	}
	if cached == nil && !force && !failedAt.IsZero() && now.Sub(failedAt) < FailureBackoff {
		return nil, fmt.Errorf("discovery of %s failed recently; not re-probing yet", domain)
	}

	descriptor, err := s.fetchDescriptor(ctx, domain)
	if err != nil {
		s.recordDiscoveryFailure(ctx, domain)
		if cached != nil {
			// A refresh that fails does not invalidate what we already trust.
			s.log.Warn("peer refresh failed, keeping the cached descriptor",
				"domain", domain, "error", err)
			return cached, nil
		}
		return nil, err
	}
	if err := s.storePeer(ctx, descriptor); err != nil {
		return nil, err
	}
	return descriptor, nil
}

// cachedPeer reads a peer's pinned descriptor out of the domain table.
func (s *Service) cachedPeer(ctx context.Context, domain string) (*Descriptor, time.Time, time.Time, error) {
	var (
		publicKey, previous, versions string
		registrar                     bool
		envelopeCap, attachmentCap    int64
		discoveredAt, failedAt        sql.NullTime
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT "public_key", "previous_public_keys", "versions", "is_registrar", `+
			`"max_envelope_bytes", "max_attachment_bytes", "discovered_at", "discovery_failed_at" `+
			`FROM "domain" WHERE "name" = ? AND "is_local" = FALSE LIMIT 1`, domain).
		Scan(&publicKey, &previous, &versions, &registrar, &envelopeCap, &attachmentCap,
			&discoveredAt, &failedAt)
	if err == sql.ErrNoRows {
		return nil, time.Time{}, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, time.Time{}, fmt.Errorf("look up peer %s: %w", domain, err)
	}
	if publicKey == "" {
		return nil, time.Time{}, failedAt.Time, nil
	}
	return &Descriptor{
		Domain:             domain,
		PublicKey:          publicKey,
		PreviousPublicKeys: splitList(previous),
		Versions:           splitList(versions),
		Registrar:          registrar,
		MaxEnvelopeBytes:   envelopeCap,
		MaxAttachmentBytes: attachmentCap,
	}, discoveredAt.Time, failedAt.Time, nil
}

// storePeer pins a descriptor. This is the trust-on-first-use step: the Web PKI
// certificate authenticated the hostname at first contact, and from here the
// pinned key authenticates every envelope regardless of what the CA does later.
func (s *Service) storePeer(ctx context.Context, d *Descriptor) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO "domain" ("name", "is_local", "public_key", "previous_public_keys", "versions", `+
			`"is_registrar", "max_envelope_bytes", "max_attachment_bytes", "discovered_at") `+
			"VALUES (?,FALSE,?,?,?,?,?,?,?) "+
			`ON CONFLICT ("name") DO UPDATE SET "public_key" = "excluded"."public_key", `+
			`"previous_public_keys" = "excluded"."previous_public_keys", "versions" = "excluded"."versions", `+
			`"is_registrar" = "excluded"."is_registrar", "max_envelope_bytes" = "excluded"."max_envelope_bytes", `+
			`"max_attachment_bytes" = "excluded"."max_attachment_bytes", `+
			`"discovered_at" = "excluded"."discovered_at", "discovery_failed_at" = NULL`,
		d.Domain, d.PublicKey, strings.Join(d.PreviousPublicKeys, ","), strings.Join(d.Versions, ","),
		d.Registrar, d.EnvelopeCap(), d.AttachmentCap(), s.db.Now())
	if err != nil {
		return fmt.Errorf("cache peer %s: %w", d.Domain, err)
	}
	return nil
}

func (s *Service) recordDiscoveryFailure(ctx context.Context, domain string) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO "domain" ("name", "is_local", "discovery_failed_at") VALUES (?,FALSE,?) `+
			`ON CONFLICT ("name") DO UPDATE SET "discovery_failed_at" = "excluded"."discovery_failed_at"`,
		domain, s.db.Now()); err != nil {
		s.log.Warn("could not record a discovery failure", "domain", domain, "error", err)
	}
}

// RegisterLocalDomains writes a domain row for each configured local Domain, so
// the database agrees with the configuration about what this Server hosts. This
// retires the old "the row where customer_id IS NULL is the domain" convention.
func (s *Service) RegisterLocalDomains(ctx context.Context) error {
	for _, d := range s.local {
		public := EncodePublicKey(d.key.Public().(ed25519.PublicKey))
		var customerID any
		if d.CustomerID != 0 {
			customerID = d.CustomerID
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO "domain" ("name", "customer_id", "is_local", "public_key", "versions", "discovered_at") `+
				"VALUES (?,?,TRUE,?,?,?) "+
				`ON CONFLICT ("name") DO UPDATE SET "customer_id" = "excluded"."customer_id", "is_local" = TRUE, `+
				`"public_key" = "excluded"."public_key", "versions" = "excluded"."versions", `+
				`"discovered_at" = "excluded"."discovered_at"`,
			d.Domain, customerID, public, Version, s.db.Now()); err != nil {
			return fmt.Errorf("register local domain %s: %w", d.Domain, err)
		}
	}
	return nil
}

func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
