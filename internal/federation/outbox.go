package federation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/103mail/webmail-server/internal/store"
)

// Outbound is one envelope the mail domain wants delivered to one peer. It is
// what email.Service hands over inside the transaction that writes the g_* rows.
type Outbound struct {
	PeerDomain   string
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
	Members      []string
	Refused      []string
	Left         []string
	Attachments  []OutboundAttachment
	// RelatedID is the local g_message id, kept so a bounce can name the
	// message that failed.
	RelatedID int64
}

// OutboundAttachment carries raw bytes; the envelope encodes them.
type OutboundAttachment struct {
	FederationID string
	FileName     string
	MimeType     string
	Data         []byte
}

// Enqueue writes an envelope to the outbox inside the caller's transaction.
//
// This is the write-before-send rule, and it is the whole reliability story: no
// row, no send attempt, and a row that commits is a send that will happen
// eventually. It is what lets a compose return 201 to the browser before a
// single packet has gone to a peer (RFC 6.4, design 4).
func (s *Service) Enqueue(ctx context.Context, tx *sql.Tx, out Outbound) error {
	if !s.Enabled() {
		return fmt.Errorf("federation is not configured")
	}
	envelopeID, err := NewUUID()
	if err != nil {
		return err
	}
	envelope := Envelope{
		Version:      Version,
		EnvelopeID:   envelopeID,
		OriginDomain: out.OriginDomain,
		OriginSentAt: FormatTimestamp(s.db.Now()),
		Thread: Thread{
			FederationID: out.ThreadFedID,
			Subject:      out.Subject,
			Members:      out.Members,
			Refused:      out.Refused,
			Left:         out.Left,
		},
		Message: Message{
			FederationID: out.MessageFedID,
			SenderEmail:  out.SenderEmail,
			SenderName:   out.SenderName,
			Body:         out.Body,
			BodyHTML:     out.BodyHTML,
			SentAt:       FormatTimestamp(out.SentAt),
		},
		Attachments: make([]Attachment, 0, len(out.Attachments)),
		Recipients:  out.Recipients,
	}
	if out.InReplyTo != "" {
		inReplyTo := out.InReplyTo
		envelope.Thread.InReplyTo = &inReplyTo
	}
	for _, a := range out.Attachments {
		envelope.Attachments = append(envelope.Attachments, encodeAttachment(a))
	}

	// Serialized once, here, and stored as bytes. Retries reuse these exact
	// bytes rather than re-serializing, because JSON key ordering is not
	// guaranteed to be stable across runs and the signature covers the body.
	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("serialize envelope: %w", err)
	}

	var relatedID any
	if out.RelatedID != 0 {
		relatedID = out.RelatedID
	}
	now := s.db.Now()
	// The body is UTF-8 JSON and goes in as text. Passed as bytes, SQLite
	// would store a blob in the text column.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "federation_outbox" ("kind", "peer_domain", "origin_domain", "body", `+
			`"idempotency_key", "related_id", "status", "next_retry_at", "created_at") `+
			"VALUES ('envelope',?,?,?,?,?, 'pending', ?, ?)",
		out.PeerDomain, out.OriginDomain, string(body), envelopeID, relatedID, now, now); err != nil {
		return fmt.Errorf("enqueue envelope for %s: %w", out.PeerDomain, err)
	}
	return nil
}

// outboxRow is one claimed piece of work.
type outboxRow struct {
	ID           int64
	PeerDomain   string
	OriginDomain string
	Body         []byte
	EnvelopeID   string
	Attempts     int
	CreatedAt    time.Time
}

// Drain sends everything that is due and returns how many rows it handled.
//
// Rows are claimed one peer at a time and each peer is processed in its own
// pass, so one slow or dead Domain delays only the envelopes addressed to it.
// There is no batch and no cross-peer ordering: five destinations are five
// independent deliveries (design 4).
func (s *Service) Drain(ctx context.Context, limit int) (int, error) {
	if err := s.Backfill(ctx); err != nil {
		s.log.Error("backfill failed", "error", err)
	}
	peers, err := s.duePeers(ctx)
	if err != nil {
		return 0, err
	}
	handled := 0
	for _, peer := range peers {
		if ctx.Err() != nil {
			return handled, ctx.Err()
		}
		rows, err := s.claim(ctx, peer, limit)
		if err != nil {
			s.log.Error("could not claim outbox rows", "peer", peer, "error", err)
			continue
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return handled, ctx.Err()
			}
			s.attempt(ctx, row)
			handled++
		}
	}
	return handled, nil
}

func (s *Service) duePeers(ctx context.Context) ([]string, error) {
	now := s.db.Now()
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT "peer_domain" FROM "federation_outbox" `+
			`WHERE "status" = 'pending' AND "next_retry_at" <= ? `+
			`AND ("claimed_until" IS NULL OR "claimed_until" < ?)`, now, now)
	if err != nil {
		return nil, fmt.Errorf("list peers with work: %w", err)
	}
	defer rows.Close()
	var peers []string
	for rows.Next() {
		var peer string
		if err := rows.Scan(&peer); err != nil {
			return nil, fmt.Errorf("scan peer: %w", err)
		}
		peers = append(peers, peer)
	}
	return peers, rows.Err()
}

// claim leases due rows to this worker before doing any network work, so two
// workers cannot send the same envelope twice.
//
// The lease is one UPDATE that re-checks the claim in its own WHERE clause. On
// PostgreSQL a second worker that waited on the row lock re-evaluates that
// condition against the row the first one wrote, finds the lease taken and
// skips the row; SQLite has a single writer, so the question does not arise. A
// worker that dies holding a lease loses it when it expires, and the row is
// claimable again: nothing has to notice the death (ADR 19).
func (s *Service) claim(ctx context.Context, peer string, limit int) ([]outboxRow, error) {
	now := s.db.Now()
	rows, err := s.db.QueryContext(ctx,
		`UPDATE "federation_outbox" SET "claimed_by" = ?, "claimed_until" = ? `+
			`WHERE "id" IN (SELECT "id" FROM "federation_outbox" `+
			`WHERE "status" = 'pending' AND "next_retry_at" <= ? AND "peer_domain" = ? `+
			`AND ("claimed_until" IS NULL OR "claimed_until" < ?) `+
			`ORDER BY "next_retry_at", "id" LIMIT ?) `+
			`AND "status" = 'pending' AND ("claimed_until" IS NULL OR "claimed_until" < ?) `+
			`RETURNING "id", "peer_domain", "origin_domain", "body", "idempotency_key", "attempts", "created_at"`,
		s.worker, now.Add(Lease), now, peer, now, limit, now)
	if err != nil {
		return nil, fmt.Errorf("claim outbox rows: %w", err)
	}
	defer rows.Close()
	var claimed []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.ID, &r.PeerDomain, &r.OriginDomain, &r.Body,
			&r.EnvelopeID, &r.Attempts, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		claimed = append(claimed, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim outbox rows: %w", err)
	}
	// RETURNING promises no order; send oldest first.
	sort.Slice(claimed, func(i, j int) bool { return claimed[i].ID < claimed[j].ID })
	return claimed, nil
}

// attempt makes one delivery attempt and records what happened.
func (s *Service) attempt(ctx context.Context, row outboxRow) {
	// A row whose budget has run out is not tried again (RFC 6.4).
	if s.db.Now().Sub(row.CreatedAt) >= s.cfg.RetryBudget {
		s.owe(ctx, row, 0)
		return
	}
	status, retryAfter, refused, err := s.post(ctx, row)

	switch {
	case err == nil && status == http.StatusAccepted:
		s.finish(ctx, row, "delivered", status, "")
		s.log.Info("envelope delivered", "peer", row.PeerDomain, "envelope", row.EnvelopeID)
		if len(refused) > 0 {
			s.bounceRefused(ctx, row, refused)
		}

	case isPermanent(status):
		// The bounce names the error code the peer answered with, or the one
		// we refused the send with ourselves (RFC 6.5).
		code := fmt.Sprintf("peer answered %d", status)
		var answer *Rejection
		if errors.As(err, &answer) && answer.Code != "" {
			code = answer.Code
		} else if err != nil {
			code = err.Error()
		}
		if err != nil {
			s.log.Warn("envelope refused", "peer", row.PeerDomain, "envelope", row.EnvelopeID, "error", err)
		}
		s.fail(ctx, row, status, code)

	case s.db.Now().Sub(row.CreatedAt) >= s.cfg.RetryBudget:
		// The 72-hour budget is a hard stop; retrying past it is forbidden and
		// the sender is owed an answer.
		s.owe(ctx, row, status)

	default:
		detail := fmt.Sprintf("peer answered %d", status)
		if err != nil {
			detail = err.Error()
		}
		s.reschedule(ctx, row, status, detail, retryAfter)
	}
}

// post signs and sends. The signature is computed here rather than at enqueue
// time because the signing input covers the Date and Nonce headers, which have
// to be fresh on every attempt: a signature made three days ago would be
// rejected as a stale request. The body bytes are the stored ones, untouched.
func (s *Service) post(ctx context.Context, row outboxRow) (int, time.Duration, []Refusal, error) {
	origin, ok := s.local[row.OriginDomain]
	if !ok {
		return 0, 0, nil, fmt.Errorf("origin domain %s is no longer hosted here", row.OriginDomain)
	}

	peer, err := s.Peer(ctx, row.PeerDomain, false)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("discover %s: %w", row.PeerDomain, err)
	}
	if !peer.Speaks(Version) {
		// Permanent: the peer has told us it does not speak our version, and
		// no amount of retrying changes that.
		return http.StatusBadRequest, 0, nil, reject(http.StatusBadRequest, ErrUnsupportedVersion,
			fmt.Sprintf("peer %s does not speak %s", row.PeerDomain, Version))
	}
	// Both caps are the peer's, and sending past either is forbidden, so a
	// message that does not fit is refused here as the peer would (RFC 7).
	if int64(len(row.Body)) > peer.EnvelopeCap() {
		return http.StatusRequestEntityTooLarge, 0, nil, reject(http.StatusRequestEntityTooLarge, ErrTooLarge,
			fmt.Sprintf("envelope is %d bytes, peer %s accepts %d", len(row.Body), row.PeerDomain, peer.EnvelopeCap()))
	}
	if size := largestAttachment(row.Body); size > peer.AttachmentCap() {
		return http.StatusRequestEntityTooLarge, 0, nil, reject(http.StatusRequestEntityTooLarge, ErrTooLarge,
			fmt.Sprintf("an attachment is %d bytes, peer %s accepts %d", size, row.PeerDomain, peer.AttachmentCap()))
	}

	nonce, err := NewNonce()
	if err != nil {
		return 0, 0, nil, err
	}
	date := HTTPDate(time.Now())
	signature := Sign(origin.key, Version, origin.Domain, nonce, date, row.Body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		BaseURL(row.PeerDomain)+InboundPath, bytes.NewReader(row.Body))
	if err != nil {
		return 0, 0, nil, fmt.Errorf("build inbound request: %w", err)
	}
	req.ContentLength = int64(len(row.Body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Date", date)
	req.Header.Set("X-Federation-Version", Version)
	req.Header.Set("X-Federation-Sender", origin.Domain)
	req.Header.Set("X-Federation-Nonce", nonce)
	req.Header.Set("X-Federation-Signature", signature)

	resp, err := s.deliveryClient.Do(req)
	if err != nil {
		// Connection refused, TLS failure, timeout: all transient (RFC 6.4).
		return 0, 0, nil, err
	}
	defer resp.Body.Close()
	// The body is read in full for the connection to be reusable. An error
	// body is only logged; a 202 may list up to 100 refused recipients.
	detail, _ := readCapped(resp.Body, 64<<10)

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return http.StatusBadRequest, 0, nil, fmt.Errorf("peer %s redirected inbound, which is not honoured", row.PeerDomain)
	}
	if resp.StatusCode == http.StatusAccepted {
		return resp.StatusCode, 0, refusalsIn([]byte(detail)), nil
	}
	// A signature the peer refuses may just mean it holds a stale key for us.
	// Refreshing our own descriptor is not possible from here, but forcing a
	// re-read of theirs costs one request and fixes the mirror-image case.
	if resp.StatusCode == http.StatusUnauthorized {
		if _, refreshErr := s.Peer(ctx, row.PeerDomain, true); refreshErr != nil {
			s.log.Warn("forced peer refresh failed", "peer", row.PeerDomain, "error", refreshErr)
		}
	}
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	return resp.StatusCode, retryAfterOf(resp), nil, answerOf(resp.StatusCode, row.PeerDomain, detail)
}

// answerOf reads a peer's error answer into a Rejection, so that the code it
// gave reaches the sender's bounce (RFC 6.3, 6.5). A body that is not the
// RFC's {"error": ...} keeps only the status.
func answerOf(status int, peer, body string) *Rejection {
	var answer struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal([]byte(body), &answer)
	detail := fmt.Sprintf("peer %s answered %d: %s", peer, status, strings.TrimSpace(body))
	return reject(status, answer.Error, detail)
}

// largestAttachment is the decoded size of the biggest attachment an
// envelope declares. Only the sizes are read; the envelope is our own.
func largestAttachment(body []byte) int64 {
	var envelope struct {
		Attachments []struct {
			FileSize int64 `json:"file_size"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0
	}
	var largest int64
	for _, a := range envelope.Attachments {
		largest = max(largest, a.FileSize)
	}
	return largest
}

// refusalsIn reads the refused recipients out of a 202 body. A body without
// the list, from a 0.2 peer, or one that is not JSON, refuses nobody: the 202
// itself is what counts.
func refusalsIn(body []byte) []Refusal {
	var a Accepted
	if err := json.Unmarshal(body, &a); err != nil {
		return nil
	}
	for i := range a.Refused {
		a.Refused[i].Address = strings.ToLower(strings.TrimSpace(a.Refused[i].Address))
	}
	return a.Refused
}

// bounceable groups refused addresses by error code, keeping only addresses
// the envelope was actually sent to, so a peer cannot cause bounces for
// addresses it never received (RFC 6.5).
func bounceable(envelope Envelope, refused []Refusal) map[string][]string {
	out := map[string][]string{}
	for _, r := range refused {
		if !slices.ContainsFunc(envelope.Recipients, func(a string) bool { return strings.EqualFold(a, r.Address) }) ||
			slices.Contains(out[r.Error], r.Address) {
			continue
		}
		out[r.Error] = append(out[r.Error], r.Address)
	}
	return out
}

// bounceRefused tells the sender about the recipients a peer accepted the
// envelope without, one bounce per reason (RFC 6.5).
func (s *Service) bounceRefused(ctx context.Context, row outboxRow, refused []Refusal) {
	if s.deliverer == nil {
		return
	}
	var envelope Envelope
	if err := json.Unmarshal(row.Body, &envelope); err != nil {
		s.log.Error("could not read a delivered envelope to bounce its refusals", "id", row.ID, "error", err)
		return
	}
	for code, addresses := range bounceable(envelope, refused) {
		if err := s.deliverer.Bounce(ctx, BounceNotice{
			OriginDomain: row.OriginDomain,
			SenderEmail:  envelope.Message.SenderEmail,
			Subject:      envelope.Thread.Subject,
			ThreadFedID:  envelope.Thread.FederationID,
			Refused:      code == ErrUnknownRecipient,
			Recipients:   addresses,
			PeerDomain:   row.PeerDomain,
			EnvelopeID:   row.EnvelopeID,
			HTTPStatus:   http.StatusAccepted,
			ErrorCode:    code,
		}); err != nil {
			s.log.Error("could not write a bounce", "envelope", row.EnvelopeID, "error", err)
		}
	}
}

// isPermanent classifies a response. Everything the RFC lists as permanent is
// permanent; 0 means we never got a status, which is a transport error and so
// transient.
func isPermanent(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType:
		return true
	}
	return false
}

func retryAfterOf(resp *http.Response) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if seconds, err := strconv.Atoi(v); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return 0
}

func (s *Service) finish(ctx context.Context, row outboxRow, status string, httpStatus int, detail string) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE "federation_outbox" SET "status" = ?, "attempts" = "attempts" + 1, `+
			`"last_http_status" = ?, "last_error" = ?, "delivered_at" = ?, `+
			`"claimed_by" = NULL, "claimed_until" = NULL WHERE "id" = ?`,
		status, nullableStatus(httpStatus), nullableDetail(detail), s.db.Now(), row.ID); err != nil {
		s.log.Error("could not record delivery", "id", row.ID, "error", err)
	}
}

// reschedule backs off with full jitter, capped at an hour. Full jitter rather
// than plain exponential so that a peer coming back up is not hit by every
// queued sender at the same instant.
func (s *Service) reschedule(ctx context.Context, row outboxRow, httpStatus int, detail string, retryAfter time.Duration) {
	delay := backoff(row.Attempts + 1)
	if retryAfter > delay {
		// A peer that told us how long to wait is obeyed, even past the cap.
		delay = retryAfter
	}
	// Never past the budget: the row is looked at again when it runs out, so
	// the sender hears then and not a backoff later.
	if deadline := row.CreatedAt.Add(s.cfg.RetryBudget); s.db.Now().Add(delay).After(deadline) {
		delay = deadline.Sub(s.db.Now())
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE "federation_outbox" SET "status" = 'pending', "attempts" = "attempts" + 1, `+
			`"last_http_status" = ?, "last_error" = ?, "next_retry_at" = ?, `+
			`"claimed_by" = NULL, "claimed_until" = NULL WHERE "id" = ?`,
		nullableStatus(httpStatus), nullableDetail(detail), s.db.Now().Add(delay), row.ID); err != nil {
		s.log.Error("could not reschedule an envelope", "id", row.ID, "error", err)
		return
	}
	s.log.Warn("envelope delivery failed, will retry",
		"peer", row.PeerDomain, "envelope", row.EnvelopeID, "attempt", row.Attempts+1,
		"in", delay.String(), "detail", detail)
}

// backoff is exponential with base 30s and full jitter, capped at one hour.
func backoff(attempt int) time.Duration {
	delay := RetryBase << min(attempt-1, 12)
	if delay > RetryCap || delay <= 0 {
		delay = RetryCap
	}
	// #nosec G404 -- jitter spreads retries; it is not a security decision.
	return time.Duration(rand.Int63n(int64(delay)) + int64(RetryBase))
}

// fail retires a row and tells the sender. A permanent failure that stayed
// silent would leave someone believing a message arrived, so the bounce is not
// optional (RFC 6.5).
func (s *Service) fail(ctx context.Context, row outboxRow, httpStatus int, detail string) {
	s.finish(ctx, row, "permanent_failure", httpStatus, detail)
	s.log.Error("envelope permanently failed",
		"peer", row.PeerDomain, "envelope", row.EnvelopeID, "status", httpStatus, "detail", detail)
	// 404 is only ever no_recipients: none of them exist there (RFC 6.3).
	s.bounce(ctx, row, httpStatus, detail, httpStatus == http.StatusNotFound, false)
}

// owe retires a row that used up its retry budget without a response. The
// peer may only have been away, so the message is kept and sent again once it
// answers (Backfill), and the sender is told both things (RFC 6.4, 6.5).
func (s *Service) owe(ctx context.Context, row outboxRow, httpStatus int) {
	s.finish(ctx, row, "owed", httpStatus, "timed_out")
	s.log.Warn("envelope timed out, owed until the peer answers",
		"peer", row.PeerDomain, "envelope", row.EnvelopeID)
	s.bounce(ctx, row, httpStatus, "timed_out", false, true)
}

// bounce tells the sender of a row's message that it did not arrive.
func (s *Service) bounce(ctx context.Context, row outboxRow, httpStatus int, detail string, refused, backfill bool) {
	if s.deliverer == nil {
		return
	}
	var envelope Envelope
	if err := json.Unmarshal(row.Body, &envelope); err != nil {
		s.log.Error("could not read a failed envelope to bounce it", "id", row.ID, "error", err)
		return
	}
	if err := s.deliverer.Bounce(ctx, BounceNotice{
		OriginDomain: row.OriginDomain,
		SenderEmail:  envelope.Message.SenderEmail,
		Subject:      envelope.Thread.Subject,
		ThreadFedID:  envelope.Thread.FederationID,
		Refused:      refused,
		Backfill:     backfill,
		Recipients:   envelope.Recipients,
		PeerDomain:   row.PeerDomain,
		EnvelopeID:   row.EnvelopeID,
		HTTPStatus:   httpStatus,
		ErrorCode:    detail,
	}); err != nil {
		s.log.Error("could not write a bounce", "envelope", row.EnvelopeID, "error", err)
	}
}

// Sweep deletes state that has aged out: dedup records past their retention,
// nonces past the replay window, and delivered outbox rows.
func (s *Service) Sweep(ctx context.Context) error {
	now := s.db.Now()
	statements := []struct {
		query  string
		cutoff time.Time
	}{
		{`DELETE FROM "federation_inbox" WHERE "received_at" < ?`, now.Add(-DedupRetention)},
		{`DELETE FROM "federation_nonce" WHERE "seen_at" < ?`, now.Add(-NonceWindow)},
		{`DELETE FROM "federation_outbox" WHERE "status" = 'delivered' AND "delivered_at" < ?`,
			now.Add(-DedupRetention)},
	}
	for _, s2 := range statements {
		if _, err := s.db.ExecContext(ctx, s2.query, s2.cutoff); err != nil {
			return fmt.Errorf("sweep federation state: %w", err)
		}
	}
	return nil
}

func encodeAttachment(a OutboundAttachment) Attachment {
	return Attachment{
		FederationID: a.FederationID,
		FileName:     a.FileName,
		MimeType:     a.MimeType,
		FileSize:     int64(len(a.Data)),
		HashSHA256:   sha256Hex(a.Data),
		Data:         base64Std(a.Data),
	}
}

func nullableStatus(status int) any {
	if status == 0 {
		return nil
	}
	return status
}

func nullableDetail(detail string) any {
	if detail == "" {
		return nil
	}
	if r := []rune(detail); len(r) > 2000 {
		detail = string(r[:2000])
	}
	return detail
}

// OutboxEntry is one outbox row as an operator sees it: where it is going,
// for whom, and how far it got. It carries no message content.
type OutboxEntry struct {
	ID           int64      `json:"id"`
	Kind         string     `json:"kind"`
	PeerDomain   string     `json:"peerDomain"`
	OriginDomain string     `json:"originDomain"`
	Recipients   []string   `json:"recipients"`
	Status       string     `json:"status"`
	Attempts     int        `json:"attempts"`
	CreatedAt    time.Time  `json:"createdAt"`
	DeliveredAt  *time.Time `json:"deliveredAt"`
}

// Outbox lists the outbox, oldest first. Delivered rows stay listed until
// Sweep removes them.
func (s *Service) Outbox(ctx context.Context) ([]OutboxEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT "id", "kind", "peer_domain", "origin_domain", "body", "status", "attempts", `+
			`"created_at", "delivered_at" FROM "federation_outbox" ORDER BY "id"`)
	if err != nil {
		return nil, fmt.Errorf("list outbox: %w", err)
	}
	defer rows.Close()
	entries := []OutboxEntry{}
	for rows.Next() {
		var e OutboxEntry
		var body string
		var delivered sql.NullTime
		if err := rows.Scan(&e.ID, &e.Kind, &e.PeerDomain, &e.OriginDomain, &body, &e.Status,
			&e.Attempts, &e.CreatedAt, &delivered); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		if delivered.Valid {
			e.DeliveredAt = &delivered.Time
		}
		// Only the recipients are read out of the stored envelope.
		var envelope struct {
			Recipients []string `json:"recipients"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			return nil, fmt.Errorf("read recipients of outbox row %d: %w", e.ID, err)
		}
		e.Recipients = envelope.Recipients
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Backfill sends again the messages owed to peers that answer again. A peer
// is checked at most once per BackfillProbe, so one that stays down costs a
// descriptor request now and then and nothing more. Each owed message goes
// out as a new envelope with a new id and a budget of its own, with the same
// thread, message and recipients, and the owed row is marked backfilled so it
// is sent again only once (RFC 6.4).
func (s *Service) Backfill(ctx context.Context) error {
	peers, err := store.SelectStrings(ctx, s.db,
		`SELECT DISTINCT "peer_domain" FROM "federation_outbox" WHERE "status" = 'owed'`)
	if err != nil {
		return fmt.Errorf("list owed peers: %w", err)
	}
	now := s.db.Now()
	for _, peer := range peers {
		s.probedMu.Lock()
		last, seen := s.probed[peer]
		due := !seen || now.Sub(last) >= s.cfg.BackfillProbe
		if due {
			s.probed[peer] = now
		}
		s.probedMu.Unlock()
		if !due || !s.reachable(ctx, peer) {
			continue
		}
		if err := s.requeueOwed(ctx, peer); err != nil {
			return err
		}
	}
	return nil
}

// requeueOwed turns every row owed to a peer into a new pending envelope.
func (s *Service) requeueOwed(ctx context.Context, peer string) error {
	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT "id", "origin_domain", "body", "related_id" FROM "federation_outbox" `+
				`WHERE "status" = 'owed' AND "peer_domain" = ? ORDER BY "id"`, peer)
		if err != nil {
			return fmt.Errorf("list owed envelopes: %w", err)
		}
		type owed struct {
			id        int64
			origin    string
			body      string
			relatedID sql.NullInt64
		}
		var all []owed
		for rows.Next() {
			var o owed
			if err := rows.Scan(&o.id, &o.origin, &o.body, &o.relatedID); err != nil {
				rows.Close()
				return fmt.Errorf("scan owed envelope: %w", err)
			}
			all = append(all, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list owed envelopes: %w", err)
		}

		now := s.db.Now()
		for _, o := range all {
			var envelope Envelope
			if err := json.Unmarshal([]byte(o.body), &envelope); err != nil {
				return fmt.Errorf("read owed envelope %d: %w", o.id, err)
			}
			envelopeID, err := NewUUID()
			if err != nil {
				return err
			}
			envelope.EnvelopeID = envelopeID
			envelope.OriginSentAt = FormatTimestamp(now)
			body, err := json.Marshal(envelope)
			if err != nil {
				return fmt.Errorf("serialize backfilled envelope: %w", err)
			}
			var relatedID any
			if o.relatedID.Valid {
				relatedID = o.relatedID.Int64
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO "federation_outbox" ("kind", "peer_domain", "origin_domain", "body", `+
					`"idempotency_key", "related_id", "status", "next_retry_at", "created_at") `+
					"VALUES ('envelope',?,?,?,?,?, 'pending', ?, ?)",
				peer, o.origin, string(body), envelopeID, relatedID, now, now); err != nil {
				return fmt.Errorf("re-queue owed envelope %d: %w", o.id, err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE "federation_outbox" SET "status" = 'backfilled' WHERE "id" = ?`, o.id); err != nil {
				return fmt.Errorf("mark envelope %d backfilled: %w", o.id, err)
			}
			s.log.Info("backfilling an owed envelope", "peer", peer, "was", o.id, "envelope", envelopeID)
		}
		return nil
	})
}
