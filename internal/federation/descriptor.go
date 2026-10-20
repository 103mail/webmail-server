package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Descriptor is what a Domain publishes about itself at its Federation
// hostname (RFC 4.2). It carries no endpoint URLs: every path is fixed by the
// specification and derived from `domain`, so a peer cannot advertise a URL of
// its choosing.
type Descriptor struct {
	Domain             string   `json:"domain"`
	PublicKey          string   `json:"public_key"`
	PreviousPublicKeys []string `json:"previous_public_keys"`
	Versions           []string `json:"versions"`
	Registrar          bool     `json:"registrar"`
	MaxEnvelopeBytes   int64    `json:"max_envelope_bytes"`
	MaxAttachmentBytes int64    `json:"max_attachment_bytes"`
}

// Defaults a descriptor takes when it does not state them (RFC 4.2).
const (
	DefaultMaxEnvelopeBytes   = 10 << 20 // 10 MiB
	DefaultMaxAttachmentBytes = 25 << 20 // 25 MiB
	maxDescriptorBytes        = 65536    // RFC 4.1: cap the read, abort past it
	maxPreviousKeys           = 4
)

// Host returns the Federation hostname of a DNS domain. The `webmail.` label is
// prepended to the whole domain and never hoisted to the registrable parent, so
// a subdomain Domain is one label deeper (RFC 4.1.1).
func Host(domain string) string { return "webmail." + domain }

// BaseURL returns the root every endpoint of a Domain hangs off.
func BaseURL(domain string) string { return "https://" + Host(domain) }

// DomainFromHost turns a Federation hostname back into the DNS domain it
// serves. Every endpoint lives at webmail.{domain} and nowhere else, so
// anything not carrying that label is not a Domain of ours (RFC 1.3).
func DomainFromHost(host string) string {
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return strings.TrimPrefix(host, "webmail.")
}

// Validate applies the rejection rules in RFC 4.2. fetchedFrom is the DNS
// domain the descriptor was fetched for; a descriptor claiming to speak for
// another domain is refused, which is what stops one peer answering for
// another.
func (d *Descriptor) Validate(fetchedFrom string) error {
	if d.Domain == "" || !strings.EqualFold(d.Domain, fetchedFrom) {
		return fmt.Errorf("descriptor claims domain %q but was fetched for %q", d.Domain, fetchedFrom)
	}
	if _, err := DecodePublicKey(d.PublicKey); err != nil {
		return fmt.Errorf("descriptor public_key: %w", err)
	}
	if len(d.PreviousPublicKeys) > maxPreviousKeys {
		return fmt.Errorf("descriptor carries %d previous keys, at most %d permitted",
			len(d.PreviousPublicKeys), maxPreviousKeys)
	}
	for _, key := range d.PreviousPublicKeys {
		if _, err := DecodePublicKey(key); err != nil {
			return fmt.Errorf("descriptor previous_public_keys: %w", err)
		}
	}
	if len(d.Versions) == 0 {
		return fmt.Errorf("descriptor advertises no versions")
	}
	return nil
}

// Keys returns the whole accepted keyset: the current key first, then any
// rotation keys. A verifier must accept a match against any of them (RFC 6.2).
func (d *Descriptor) Keys() []ed25519.PublicKey {
	keys := make([]ed25519.PublicKey, 0, 1+len(d.PreviousPublicKeys))
	if key, err := DecodePublicKey(d.PublicKey); err == nil {
		keys = append(keys, key)
	}
	for _, encoded := range d.PreviousPublicKeys {
		if key, err := DecodePublicKey(encoded); err == nil {
			keys = append(keys, key)
		}
	}
	return keys
}

// Speaks reports whether the Domain accepts a protocol version.
func (d *Descriptor) Speaks(version string) bool {
	for _, v := range d.Versions {
		if v == version {
			return true
		}
	}
	return false
}

// EnvelopeCap and AttachmentCap apply the RFC's defaults for an absent field.
func (d *Descriptor) EnvelopeCap() int64 {
	if d.MaxEnvelopeBytes <= 0 {
		return DefaultMaxEnvelopeBytes
	}
	return d.MaxEnvelopeBytes
}

func (d *Descriptor) AttachmentCap() int64 {
	if d.MaxAttachmentBytes <= 0 {
		return DefaultMaxAttachmentBytes
	}
	return d.MaxAttachmentBytes
}

// fetchDescriptor performs discovery: a plain GET of the fixed well-known URL
// at the peer's Federation hostname.
//
// The rules here are all refusals. No redirect is followed, because a 3xx means
// discovery failed rather than moved (RFC 4.1). The read is capped, so a peer
// cannot stream an unbounded body. The URL is built from the domain rather than
// taken from configuration, so there is nothing to point somewhere else. TLS
// verification is the caller's http.Client and is never relaxed here.
func (s *Service) fetchDescriptor(ctx context.Context, domain string) (*Descriptor, error) {
	url := BaseURL(domain) + WellKnownPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build discovery request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.discoveryClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("discovery of %s was redirected, which is not honoured", domain)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery of %s answered %d", domain, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDescriptorBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read descriptor of %s: %w", domain, err)
	}
	if len(body) > maxDescriptorBytes {
		return nil, fmt.Errorf("descriptor of %s exceeds %d bytes", domain, maxDescriptorBytes)
	}

	var descriptor Descriptor
	if err := json.Unmarshal(body, &descriptor); err != nil {
		return nil, fmt.Errorf("parse descriptor of %s: %w", domain, err)
	}
	if err := descriptor.Validate(domain); err != nil {
		return nil, err
	}
	return &descriptor, nil
}
