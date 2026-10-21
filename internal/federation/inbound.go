package federation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mevdschee/webmail-server/internal/store"
)

// Request is one inbound POST, reduced to what the protocol authenticates: the
// four signed headers and the body bytes exactly as they arrived.
//
// It is a plain struct rather than an *http.Request so that the whole
// verification path is testable without a server, and so that the HTTP handler
// stays a translation layer with no protocol rules of its own.
type Request struct {
	// Destination is the DNS domain the request was sent to, read from the
	// Federation hostname in Host (RFC 1.3).
	Destination     string
	ContentType     string
	ContentEncoding string
	Version         string
	Sender          string
	Nonce           string
	Date            string
	Signature       string
	Body            []byte
}

// RequestFrom reads an inbound request, refusing anything the protocol does not
// allow before a single byte of the body is trusted.
//
// The order matters. The media type and the size cap are checked first, because
// they decide whether reading the body at all is safe. Nothing is parsed as
// JSON until the signature has verified.
func RequestFrom(r *http.Request, maxBytes int64) (*Request, *Rejection) {
	mediaType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if !strings.HasPrefix(mediaType, "application/json") {
		return nil, reject(415, ErrUnsupportedMedia, "envelopes are application/json; charset=utf-8")
	}
	// The signature covers the transmitted body, so a compressed body would
	// verify against bytes the peer never signed (RFC 5.1).
	if r.Header.Get("Content-Encoding") != "" {
		return nil, reject(415, ErrUnsupportedMedia, "Content-Encoding is not permitted on the request body")
	}
	if r.ContentLength > maxBytes {
		return nil, reject(413, ErrTooLarge, "envelope exceeds the advertised maximum")
	}

	// One byte over the cap is read deliberately: it is how an unset or lying
	// Content-Length is caught without buffering an unbounded body.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, reject(400, ErrMalformedEnvelope, "could not read the request body")
	}
	if int64(len(body)) > maxBytes {
		return nil, reject(413, ErrTooLarge, "envelope exceeds the advertised maximum")
	}

	return &Request{
		Destination: DomainFromHost(r.Host),
		ContentType: mediaType,
		Version:     r.Header.Get("X-Federation-Version"),
		Sender:      strings.ToLower(strings.TrimSpace(r.Header.Get("X-Federation-Sender"))),
		Nonce:       r.Header.Get("X-Federation-Nonce"),
		Date:        r.Header.Get("Date"),
		Signature:   r.Header.Get("X-Federation-Signature"),
		Body:        body,
	}, nil
}

// Accepted is the answer to a successful inbound POST.
type Accepted struct {
	EnvelopeID string `json:"envelope_id"`
	Status     string `json:"status"`
	// Refused lists the recipients that were not delivered to, when some
	// were (RFC 6.3).
	Refused []Refusal `json:"refused_recipients,omitempty"`
}

// Refusal is one recipient a Destination did not deliver to, and why.
type Refusal struct {
	Address string `json:"address"`
	Error   string `json:"error"`
}

// ErrUnknownRecipient refuses an address on the Destination's own domain that
// has no account there (RFC 6.3).
const ErrUnknownRecipient = "unknown_recipient"

// accepted builds the 202 for an envelope, listing the refused addresses.
func accepted(envelopeID string, refused []string) *Accepted {
	a := &Accepted{EnvelopeID: envelopeID, Status: "accepted"}
	for _, address := range refused {
		a.Refused = append(a.Refused, Refusal{Address: address, Error: ErrUnknownRecipient})
	}
	return a
}

// Receive authenticates, validates and delivers an inbound envelope.
//
// The sequence is: is this peer allowed, is the request fresh, does it carry a
// signature from a key we have pinned for this sender, have we seen this nonce,
// is the envelope well formed, do the three statements of who sent it agree,
// and only then, have we already accepted it. Delivery is last.
func (s *Service) Receive(ctx context.Context, req *Request) (*Accepted, *Rejection) {
	if !s.Enabled() {
		return nil, reject(503, ErrServerError, "this Server hosts no federated Domain")
	}
	if req.Sender == "" {
		return nil, reject(401, ErrSenderMismatch, "X-Federation-Sender is missing")
	}
	if s.IsLocal(req.Sender) {
		// A peer claiming to be us is either a loop or an impersonation
		// attempt. Either way there is no key to verify it with.
		return nil, reject(403, ErrPeerNotAllowed, "that domain is hosted here")
	}
	if !s.peerAllowed(req.Sender) {
		return nil, reject(403, ErrPeerNotAllowed, "this Domain does not federate with "+req.Sender)
	}
	if req.Version != Version {
		return nil, reject(400, ErrUnsupportedVersion, "this Domain advertises only "+Version)
	}

	// Freshness before signature: a replayed request whose Date has aged out is
	// refused without spending a verification on it.
	if r := s.checkDate(req.Date); r != nil {
		return nil, r
	}
	if len(req.Nonce) == 0 || len(req.Nonce) > 64 {
		return nil, reject(401, ErrReplay, "X-Federation-Nonce is missing or oversized")
	}

	if r := s.checkSignature(ctx, req); r != nil {
		return nil, r
	}

	// The nonce is claimed only after the signature verifies, so an unsigned
	// flood cannot fill the replay table with values of its choosing.
	if r := s.claimNonce(ctx, req.Sender, "inbound", req.Nonce); r != nil {
		return nil, r
	}

	var envelope Envelope
	if err := json.Unmarshal(req.Body, &envelope); err != nil {
		return nil, reject(400, ErrMalformedEnvelope, "envelope is not JSON")
	}
	if r := envelope.Validate(s.cfg.MaxAttachmentBytes); r != nil {
		return nil, r
	}

	// The triple check (RFC 9.6): the transport says who sent this, the
	// envelope says who sent this, and the message says who wrote it. All three
	// must name the same Domain, or one peer could speak for another.
	if !strings.EqualFold(envelope.OriginDomain, req.Sender) {
		return nil, reject(401, ErrSenderMismatch, "origin_domain does not match X-Federation-Sender")
	}
	if !strings.EqualFold(DomainOf(envelope.Message.SenderEmail), req.Sender) {
		return nil, reject(401, ErrSenderMismatch, "sender_email is not on the sending Domain")
	}
	if r := envelope.RecipientsOn(req.Destination); r != nil {
		return nil, r
	}

	inbound, r := s.toInbound(&envelope)
	if r != nil {
		return nil, r
	}
	if s.deliverer == nil {
		return nil, reject(500, ErrServerError, "no mail domain is wired to receive")
	}

	// Dedup last, so a replay of an envelope we accepted replays the same 202
	// without re-running any of the work above having any effect (RFC 5.4).
	// The refused list is read again rather than stored: it is the recipients
	// with no copy of the message, and copies are only written the first time.
	seen, err := s.alreadyAccepted(ctx, req.Sender, envelope.EnvelopeID)
	if err != nil {
		return nil, reject(500, ErrServerError, err.Error())
	}
	if seen {
		refused, err := s.deliverer.Undelivered(ctx, *inbound)
		if err != nil {
			return nil, reject(500, ErrServerError, err.Error())
		}
		return accepted(envelope.EnvelopeID, refused), nil
	}

	// A replay above was answered whatever the rate; a new envelope counts.
	if r := s.rateLimited(ctx, req.Sender); r != nil {
		return nil, r
	}

	delivered, refused, err := s.deliverer.DeliverInbound(ctx, *inbound)
	if errors.Is(err, ErrNotMember) {
		// Not recorded as accepted, so an Origin whose invitation was only
		// late is let in on a retry (RFC 5.6, 6.3).
		return nil, reject(http.StatusTooEarly, ErrNotAMember, err.Error())
	}
	if err != nil {
		// A local failure is transient from the peer's point of view: it should
		// retry, and the dedup row is deliberately not written.
		s.log.Error("inbound delivery failed", "sender", req.Sender,
			"envelope", envelope.EnvelopeID, "error", err)
		return nil, reject(500, ErrServerError, "could not store the message")
	}
	// None delivered because none exist is a 404. None delivered because
	// they all left the thread is nothing to do, and no reason to bounce.
	if delivered == 0 && len(refused) > 0 {
		return nil, reject(404, ErrNoRecipients, "no recipient is known here")
	}

	if err := s.recordAccepted(ctx, req.Sender, envelope.EnvelopeID); err != nil {
		// The message is stored; failing the request now would cause a retry
		// that duplicates it. Logged and accepted instead.
		s.log.Error("could not record an accepted envelope",
			"sender", req.Sender, "envelope", envelope.EnvelopeID, "error", err)
	}
	s.log.Info("envelope accepted", "sender", req.Sender,
		"envelope", envelope.EnvelopeID, "recipients", delivered, "refused", len(refused))
	return accepted(envelope.EnvelopeID, refused), nil
}

// checkDate enforces the clock-skew window in both directions. A request from
// the future is as suspicious as one from the past.
func (s *Service) checkDate(value string) *Rejection {
	if value == "" {
		return reject(401, ErrStaleRequest, "the Date header is required")
	}
	sent, err := ParseHTTPDate(value)
	if err != nil {
		return reject(401, ErrStaleRequest, "the Date header is unparseable")
	}
	if skew := time.Since(sent); skew > ClockSkew || skew < -ClockSkew {
		return reject(401, ErrStaleRequest, "the Date header is outside the accepted window")
	}
	return nil
}

// checkSignature verifies against the sender's pinned keyset, and retries once
// against a freshly fetched descriptor.
//
// That second attempt is what makes key rotation work without coordination: a
// peer that rotated signs with a key we have not seen, the first verification
// fails, and the failure itself is the signal to refresh (RFC 4.3, 9.5).
func (s *Service) checkSignature(ctx context.Context, req *Request) *Rejection {
	peer, err := s.Peer(ctx, req.Sender, false)
	if err != nil {
		return reject(401, ErrBadSignature, "no descriptor could be resolved for "+req.Sender)
	}
	if Verify(peer.Keys(), req.Signature, req.Version, req.Sender, req.Nonce, req.Date, req.Body) {
		return nil
	}

	refreshed, err := s.Peer(ctx, req.Sender, true)
	if err != nil {
		return reject(401, ErrBadSignature, "signature did not verify and the peer could not be refreshed")
	}
	if Verify(refreshed.Keys(), req.Signature, req.Version, req.Sender, req.Nonce, req.Date, req.Body) {
		return nil
	}
	return reject(401, ErrBadSignature, "signature does not verify against any key of "+req.Sender)
}

// claimNonce records a nonce and reports a replay. The unique index does the
// deciding, so two concurrent requests carrying the same nonce cannot both pass.
func (s *Service) claimNonce(ctx context.Context, sender, endpoint, nonce string) *Rejection {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO "federation_nonce" ("sender_domain", "endpoint", "nonce", "seen_at") VALUES (?,?,?,?) `+
			`ON CONFLICT DO NOTHING`,
		sender, endpoint, nonce, s.db.Now())
	if err != nil {
		return reject(500, ErrServerError, "could not record the nonce")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return reject(500, ErrServerError, "could not record the nonce")
	}
	if affected == 0 {
		return reject(401, ErrReplay, "this nonce has already been used")
	}
	return nil
}

func (s *Service) alreadyAccepted(ctx context.Context, sender, envelopeID string) (bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT "id" FROM "federation_inbox" WHERE "sender_domain" = ? AND "envelope_id" = ? LIMIT 1`,
		sender, envelopeID).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up accepted envelope: %w", err)
	}
	return true, nil
}

// rateLimited answers 429 when a peer has had as many envelopes accepted here
// as its limit allows, over the last minute or the last day, with how many
// seconds remain until the oldest of them falls out of the window. Counting
// accepted envelopes on this Domain's clock means a peer is never charged for
// what it was refused (RFC 9.10).
func (s *Service) rateLimited(ctx context.Context, sender string) *Rejection {
	now := s.db.Now()
	for _, window := range []struct {
		limit  int
		period time.Duration
	}{
		{s.cfg.RateLimit.PerMinute, time.Minute},
		{s.cfg.RateLimit.PerDay, 24 * time.Hour},
	} {
		var count int
		var oldest store.NullTime
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*), MIN("received_at") FROM "federation_inbox" `+
				`WHERE "sender_domain" = ? AND "received_at" > ?`,
			sender, now.Add(-window.period)).Scan(&count, &oldest)
		if err != nil {
			return reject(500, ErrServerError, "could not count the peer's envelopes")
		}
		if count < window.limit || !oldest.Valid {
			continue
		}
		wait := int(oldest.Time.Add(window.period).Sub(now).Seconds())
		if wait < 1 {
			wait = 1
		}
		r := reject(429, ErrRateLimited, fmt.Sprintf("more than %d envelopes in %s", window.limit, window.period))
		r.RetryAfter = wait
		return r
	}
	return nil
}

func (s *Service) recordAccepted(ctx context.Context, sender, envelopeID string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO "federation_inbox" ("sender_domain", "envelope_id", "received_at") VALUES (?,?,?) `+
			`ON CONFLICT DO NOTHING`,
		sender, envelopeID, s.db.Now()); err != nil {
		return fmt.Errorf("record accepted envelope: %w", err)
	}
	return nil
}

// toInbound decodes what the mail domain needs, keeping only recipients that
// belong to a Domain this Server hosts.
//
// Dropping the others is not an oversight. An envelope may list recipients on
// several Domains; each Destination is told the full list so the thread's
// subscriber set is complete, but only delivers to the addresses it is
// authoritative for.
func (s *Service) toInbound(e *Envelope) (*Inbound, *Rejection) {
	sentAt, err := ParseTimestamp(e.Message.SentAt)
	if err != nil {
		return nil, reject(400, ErrSchemaViolation, "message.sent_at is not a timestamp")
	}

	in := &Inbound{
		OriginDomain: e.OriginDomain,
		ThreadFedID:  e.Thread.FederationID,
		Subject:      e.Thread.Subject,
		MessageFedID: e.Message.FederationID,
		SenderEmail:  e.Message.SenderEmail,
		SenderName:   e.Message.SenderName,
		Body:         e.Message.Body,
		BodyHTML:     e.Message.BodyHTML,
		SentAt:       sentAt,
		Recipients:   e.Recipients,
		Members:      e.Members(),
		Refused:      e.Thread.Refused,
		Left:         e.Thread.Left,
	}
	if e.Thread.InReplyTo != nil {
		in.InReplyTo = *e.Thread.InReplyTo
	}
	for i := range e.Attachments {
		data, r := e.Attachments[i].Bytes()
		if r != nil {
			return nil, r
		}
		in.Attachments = append(in.Attachments, InboundAttachment{
			FederationID: e.Attachments[i].FederationID,
			FileName:     e.Attachments[i].FileName,
			MimeType:     e.Attachments[i].MimeType,
			Data:         data,
		})
	}
	return in, nil
}

// LocalRecipients filters an address list down to the ones this Server is
// authoritative for. Exported because the mail domain decides what to do with
// them and this package holds the knowledge of which Domains are local.
func (s *Service) LocalRecipients(addresses []string) []string {
	local := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if s.IsLocal(DomainOf(address)) {
			local = append(local, address)
		}
	}
	return local
}

// MaxAttachmentBytes is the attachment cap this Domain advertises, and the
// largest file its own people may attach.
func (s *Service) MaxAttachmentBytes() int64 {
	if s.cfg == nil || s.cfg.MaxAttachmentBytes <= 0 {
		return DefaultMaxAttachmentBytes
	}
	return s.cfg.MaxAttachmentBytes
}

// MaxEnvelopeBytes is the cap this Domain advertises and enforces.
func (s *Service) MaxEnvelopeBytes() int64 {
	if s.cfg == nil || s.cfg.MaxEnvelopeBytes <= 0 {
		return DefaultMaxEnvelopeBytes
	}
	return s.cfg.MaxEnvelopeBytes
}
