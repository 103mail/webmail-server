package federation

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Version is the only protocol version this implementation speaks.
const Version = "0.2"

// InboundPath and WellKnownPath are fixed by the RFC and are never configured
// by either peer. A descriptor that tries to override an endpoint path is
// ignored, which removes a whole class of misconfiguration.
const (
	InboundPath   = "/api/v1/federation/inbound"
	WellKnownPath = "/.well-known/webmail-federation"
)

// Envelope is one message transferred between Domains (RFC 5.2).
type Envelope struct {
	Version      string       `json:"version"`
	EnvelopeID   string       `json:"envelope_id"`
	OriginDomain string       `json:"origin_domain"`
	OriginSentAt string       `json:"origin_sent_at"`
	Thread       Thread       `json:"thread"`
	Message      Message      `json:"message"`
	Attachments  []Attachment `json:"attachments"`
	Recipients   []string     `json:"recipients"`
}

// Thread carries the identity of the conversation a message belongs to. The
// federation id is stable across Domains, which is what lets a reply arriving
// from a peer land on the thread it belongs to rather than starting a new one.
type Thread struct {
	FederationID string  `json:"federation_id"`
	Subject      string  `json:"subject"`
	InReplyTo    *string `json:"in_reply_to"`
	// Members is everyone on the thread, on every Domain. Recipients are only
	// the Destination's share, so this is how a Destination learns who else a
	// reply written there has to reach (RFC 5.3).
	Members []string `json:"members,omitempty"`
	// Refused are addresses the sender added that their own Domain refused.
	// The Destination stops treating them as members, if this sender is the
	// one who added them (RFC 5.3, 6.5).
	Refused []string `json:"refused,omitempty"`
	// Left are members on the sender's own domain who left the thread; the
	// leaver normally sends the notice themselves (RFC 5.6).
	Left []string `json:"left,omitempty"`
}

// Message is the content itself.
type Message struct {
	FederationID string `json:"federation_id"`
	SenderEmail  string `json:"sender_email"`
	SenderName   string `json:"sender_name,omitempty"`
	Body         string `json:"body"`
	BodyHTML     string `json:"body_html,omitempty"`
	SentAt       string `json:"sent_at"`
}

// Attachment carries its bytes inline. There is no second fetch: once an
// envelope is accepted the Destination holds a self-contained copy, so a thread
// stays readable after the Origin is decommissioned (RFC 7).
type Attachment struct {
	FederationID string `json:"federation_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
	HashSHA256   string `json:"hash_sha256_hex"`
	Data         string `json:"data"`
}

// Field bounds from RFC 5.3 and RFC 7. Violating any of them is a
// schema_violation, not something to truncate and accept.
const (
	maxSubjectBytes    = 998
	maxAddressBytes    = 320
	maxSenderNameBytes = 255
	maxBodyBytes       = 1 << 20
	maxBodyHTMLBytes   = 4 << 20
	maxFileNameBytes   = 255
	maxMimeTypeBytes   = 255
	maxAttachments     = 64
	maxRecipients      = 100
	maxMembers         = 1000
)

// Error codes from RFC 6.3. They are the `error` field of a failure response
// and are part of the wire contract, so they are constants rather than prose.
const (
	ErrSchemaViolation     = "schema_violation"
	ErrUnsupportedVersion  = "unsupported_version"
	ErrAttachmentMismatch  = "attachment_mismatch"
	ErrBadSignature        = "bad_signature"
	ErrSenderMismatch      = "sender_mismatch"
	ErrReplay              = "replay"
	ErrStaleRequest        = "stale_request"
	ErrPeerNotAllowed      = "peer_not_allowed"
	ErrNoRecipients        = "no_recipients"
	ErrNotRegistrar        = "not_registrar"
	ErrTooLarge            = "too_large"
	ErrUnsupportedMedia    = "unsupported_media_type"
	ErrRateLimited         = "rate_limited"
	ErrServerError         = "server_error"
	ErrMalformedEnvelope   = "malformed_envelope"
	ErrUnknownOriginDomain = "unknown_origin_domain"
	// ErrNotAMember answers a message on a thread the Destination holds from
	// someone who is not a member of it. It is transient (425), because an
	// invitation may still be on its way (RFC 5.6, 6.3).
	ErrNotAMember = "not_a_member"
)

// Rejection is a protocol-level refusal: an HTTP status and the wire error code
// that goes with it. Returned rather than logged, because the status is part of
// the answer the peer needs in order to decide whether to retry.
type Rejection struct {
	Status int
	Code   string
	Detail string
}

func (r *Rejection) Error() string {
	if r.Detail == "" {
		return fmt.Sprintf("%d %s", r.Status, r.Code)
	}
	return fmt.Sprintf("%d %s: %s", r.Status, r.Code, r.Detail)
}

func reject(status int, code, detail string) *Rejection {
	return &Rejection{Status: status, Code: code, Detail: detail}
}

// Validate checks every field rule in RFC 5.3 and RFC 7, including recomputing
// each attachment's size and hash. It does not check the signature or the
// sender agreement; those need the request headers and live in verify.
func (e *Envelope) Validate(maxAttachmentBytes int64) *Rejection {
	if e.Version != Version {
		return reject(400, ErrUnsupportedVersion, "this Domain advertises only "+Version)
	}
	if !validUUID(e.EnvelopeID) {
		return reject(400, ErrSchemaViolation, "envelope_id is not a UUID")
	}
	if e.OriginDomain == "" || e.OriginDomain != strings.ToLower(e.OriginDomain) {
		return reject(400, ErrSchemaViolation, "origin_domain must be a lowercased DNS name")
	}
	if _, err := ParseTimestamp(e.OriginSentAt); err != nil {
		return reject(400, ErrSchemaViolation, "origin_sent_at is not an RFC 3339 UTC timestamp")
	}

	if !validUUID(e.Thread.FederationID) {
		return reject(400, ErrSchemaViolation, "thread.federation_id is not a UUID")
	}
	if len(e.Thread.Subject) > maxSubjectBytes {
		return reject(400, ErrSchemaViolation, "thread.subject exceeds 998 bytes")
	}
	if e.Thread.InReplyTo != nil && !validUUID(*e.Thread.InReplyTo) {
		return reject(400, ErrSchemaViolation, "thread.in_reply_to is not a UUID")
	}

	if !validUUID(e.Message.FederationID) {
		return reject(400, ErrSchemaViolation, "message.federation_id is not a UUID")
	}
	if len(e.Message.SenderEmail) > maxAddressBytes || !validAddress(e.Message.SenderEmail) {
		return reject(400, ErrSchemaViolation, "message.sender_email is not an address")
	}
	if len(e.Message.SenderName) > maxSenderNameBytes {
		return reject(400, ErrSchemaViolation, "message.sender_name exceeds 255 bytes")
	}
	if len(e.Message.Body) > maxBodyBytes {
		return reject(400, ErrSchemaViolation, "message.body exceeds 1 MiB")
	}
	if len(e.Message.BodyHTML) > maxBodyHTMLBytes {
		return reject(400, ErrSchemaViolation, "message.body_html exceeds 4 MiB")
	}
	if _, err := ParseTimestamp(e.Message.SentAt); err != nil {
		return reject(400, ErrSchemaViolation, "message.sent_at is not an RFC 3339 UTC timestamp")
	}

	if len(e.Attachments) > maxAttachments {
		return reject(400, ErrSchemaViolation, "more than 64 attachments")
	}
	for i := range e.Attachments {
		if r := e.Attachments[i].validate(maxAttachmentBytes); r != nil {
			return r
		}
	}

	if len(e.Recipients) == 0 || len(e.Recipients) > maxRecipients {
		return reject(400, ErrSchemaViolation, "recipients must hold between 1 and 100 addresses")
	}
	for _, address := range e.Recipients {
		if !validAddress(address) {
			return reject(400, ErrSchemaViolation, "recipient is not an address: "+address)
		}
	}

	if len(e.Thread.Members) > maxMembers {
		return reject(400, ErrSchemaViolation, "thread.members holds more than 1000 addresses")
	}
	for field, list := range map[string][]string{"thread.refused": e.Thread.Refused, "thread.left": e.Thread.Left} {
		if len(list) > maxRecipients {
			return reject(400, ErrSchemaViolation, field+" holds more than 100 addresses")
		}
		for _, address := range list {
			if !validAddress(address) {
				return reject(400, ErrSchemaViolation, field+" entry is not an address: "+address)
			}
			if slices.ContainsFunc(e.Thread.Members, func(m string) bool { return strings.EqualFold(m, address) }) {
				return reject(400, ErrSchemaViolation, address+" is in "+field+" and a member")
			}
		}
	}
	if len(e.Thread.Members) > 0 {
		for _, address := range e.Thread.Members {
			if !validAddress(address) {
				return reject(400, ErrSchemaViolation, "member is not an address: "+address)
			}
		}
		required := e.Recipients
		leaving := slices.ContainsFunc(e.Thread.Left, func(l string) bool { return strings.EqualFold(l, e.Message.SenderEmail) })
		if !IsMailerDaemon(e.Message.SenderEmail) && !leaving {
			required = append([]string{e.Message.SenderEmail}, required...)
		}
		for _, address := range required {
			if !slices.ContainsFunc(e.Thread.Members, func(m string) bool { return strings.EqualFold(m, address) }) {
				return reject(400, ErrSchemaViolation, "thread.members leaves out "+address)
			}
		}
	}
	return nil
}

// Members is everyone on the thread, lowercase and each once. A peer that
// leaves thread.members out means the sender and the recipients (RFC 5.3).
func (e *Envelope) Members() []string {
	members := e.Thread.Members
	if len(members) == 0 {
		members = append([]string{e.Message.SenderEmail}, e.Recipients...)
	}
	out := make([]string, 0, len(members))
	for _, m := range members {
		if m = strings.ToLower(strings.TrimSpace(m)); !IsMailerDaemon(m) && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// IsMailerDaemon reports whether an address is a Domain's mailer-daemon, which
// sends bounces and refusal notices and is never a member of a thread (RFC 6.5).
func IsMailerDaemon(address string) bool {
	local, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(address)), "@")
	return ok && local == "mailer-daemon"
}

// RecipientsOn checks that every recipient is on the Destination's own DNS
// domain. A parent and its subdomain are different Domains (RFC 4.1.1, 5.3).
// Any other address is refused as not_registrar, External or not: only a
// Registrar accepts addresses that are not its own (RFC 8.2).
func (e *Envelope) RecipientsOn(domain string) *Rejection {
	for _, address := range e.Recipients {
		if !strings.EqualFold(DomainOf(address), domain) {
			return reject(400, ErrNotRegistrar, "recipient "+address+" is not on "+domain)
		}
	}
	return nil
}

// Bytes decodes an attachment, checking that the declared size and hash match
// what actually arrived. A mismatch is the peer's error, not a corruption to
// paper over.
func (a *Attachment) Bytes() ([]byte, *Rejection) {
	raw, err := base64.StdEncoding.DecodeString(a.Data)
	if err != nil {
		return nil, reject(400, ErrSchemaViolation, "attachment data is not base64")
	}
	if int64(len(raw)) != a.FileSize {
		return nil, reject(400, ErrAttachmentMismatch, "file_size does not match the decoded length")
	}
	sum := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), a.HashSHA256) {
		return nil, reject(400, ErrAttachmentMismatch, "hash_sha256_hex does not match the decoded bytes")
	}
	return raw, nil
}

func (a *Attachment) validate(maxAttachmentBytes int64) *Rejection {
	if !validUUID(a.FederationID) {
		return reject(400, ErrSchemaViolation, "attachment.federation_id is not a UUID")
	}
	if a.FileName == "" || len(a.FileName) > maxFileNameBytes {
		return reject(400, ErrSchemaViolation, "attachment.file_name is empty or over 255 bytes")
	}
	if a.MimeType == "" || len(a.MimeType) > maxMimeTypeBytes {
		return reject(400, ErrSchemaViolation, "attachment.mime_type is empty or over 255 bytes")
	}
	if len(a.HashSHA256) != 64 {
		return reject(400, ErrSchemaViolation, "attachment.hash_sha256_hex is not 64 hex characters")
	}
	if a.FileSize < 0 || a.FileSize > maxAttachmentBytes {
		return reject(413, ErrTooLarge, "attachment exceeds the advertised maximum")
	}
	if _, r := a.Bytes(); r != nil {
		return r
	}
	return nil
}

// FormatTimestamp renders a time the way the wire wants it: RFC 3339 UTC with a
// trailing Z and millisecond precision. Higher precision is forbidden, so this
// is the only place timestamps are formatted.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ParseTimestamp accepts what FormatTimestamp produces, and second precision
// too, since millisecond precision is permitted rather than required.
func ParseTimestamp(v string) (time.Time, error) {
	if !strings.HasSuffix(v, "Z") {
		return time.Time{}, fmt.Errorf("timestamp %q is not UTC", v)
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", v, err)
	}
	return t.UTC(), nil
}

// DomainOf returns the lowercased domain part of an address.
func DomainOf(address string) string {
	at := strings.LastIndex(address, "@")
	if at < 0 {
		return ""
	}
	return strings.ToLower(address[at+1:])
}

// validAddress is a deliberately shallow addr-spec check: exactly one @, a
// non-empty local part, and a domain with a dot. Anything stricter would start
// rejecting addresses that peers legitimately hold, and the domain part is the
// only piece this protocol actually routes on.
func validAddress(address string) bool {
	if address == "" || len(address) > maxAddressBytes {
		return false
	}
	at := strings.Index(address, "@")
	if at <= 0 || at != strings.LastIndex(address, "@") {
		return false
	}
	domain := address[at+1:]
	if domain == "" || !strings.Contains(domain, ".") || strings.ContainsAny(address, " \t\r\n") {
		return false
	}
	return domain == strings.ToLower(domain)
}

// validUUID checks the canonical hyphenated lowercase form. The RFC asks for
// UUIDv7 specifically, but the version nibble carries no protocol meaning here
// and rejecting a peer's v4 would be gratuitous, so only the shape is enforced.
func validUUID(v string) bool {
	if len(v) != 36 {
		return false
	}
	for i, c := range v {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !isHex {
				return false
			}
		}
	}
	return true
}
