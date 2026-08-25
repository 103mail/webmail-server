package federation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
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
		OriginSentAt: FormatTimestamp(time.Now()),
		Thread: Thread{
			FederationID: out.ThreadFedID,
			Subject:      out.Subject,
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
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO `federation_outbox` (`kind`, `peer_domain`, `origin_domain`, `body`, "+
			"`idempotency_key`, `related_id`, `status`, `next_retry_at`) "+
			"VALUES ('envelope',?,?,?,?,?, 'pending', NOW())",
		out.PeerDomain, out.OriginDomain, body, envelopeID, relatedID); err != nil {
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
	rows, err := s.db.QueryContext(ctx,
		"SELECT DISTINCT `peer_domain` FROM `federation_outbox` "+
			"WHERE `status` = 'pending' AND `next_retry_at` <= NOW()")
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

// claim marks rows in flight before doing any network work, so two workers
// cannot send the same envelope twice.
func (s *Service) claim(ctx context.Context, peer string, limit int) ([]outboxRow, error) {
	var claimed []outboxRow
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			"SELECT `id`, `peer_domain`, `origin_domain`, `body`, `idempotency_key`, `attempts`, `created_at` "+
				"FROM `federation_outbox` WHERE `status` = 'pending' AND `next_retry_at` <= NOW() "+
				"AND `peer_domain` = ? ORDER BY `next_retry_at` LIMIT ? FOR UPDATE SKIP LOCKED",
			peer, limit)
		if err != nil {
			return fmt.Errorf("select outbox rows: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r outboxRow
			if err := rows.Scan(&r.ID, &r.PeerDomain, &r.OriginDomain, &r.Body,
				&r.EnvelopeID, &r.Attempts, &r.CreatedAt); err != nil {
				return fmt.Errorf("scan outbox row: %w", err)
			}
			claimed = append(claimed, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range claimed {
			if _, err := tx.ExecContext(ctx,
				"UPDATE `federation_outbox` SET `status` = 'in_flight' WHERE `id` = ?", r.ID); err != nil {
				return fmt.Errorf("claim outbox row: %w", err)
			}
		}
		return nil
	})
	return claimed, err
}

// attempt makes one delivery attempt and records what happened.
func (s *Service) attempt(ctx context.Context, row outboxRow) {
	status, retryAfter, err := s.post(ctx, row)

	switch {
	case err == nil && status == http.StatusAccepted:
		s.finish(ctx, row, "delivered", status, "")
		s.log.Info("envelope delivered", "peer", row.PeerDomain, "envelope", row.EnvelopeID)

	case isPermanent(status):
		detail := fmt.Sprintf("peer answered %d", status)
		if err != nil {
			detail = err.Error()
		}
		s.fail(ctx, row, status, detail)

	case time.Since(row.CreatedAt) >= RetryBudget:
		// The 72-hour budget is a hard stop; retrying past it is forbidden and
		// the sender is owed an answer.
		s.fail(ctx, row, status, "timed_out")

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
func (s *Service) post(ctx context.Context, row outboxRow) (int, time.Duration, error) {
	origin, ok := s.local[row.OriginDomain]
	if !ok {
		return 0, 0, fmt.Errorf("origin domain %s is no longer hosted here", row.OriginDomain)
	}

	peer, err := s.Peer(ctx, row.PeerDomain, false)
	if err != nil {
		return 0, 0, fmt.Errorf("discover %s: %w", row.PeerDomain, err)
	}
	if !peer.Speaks(Version) {
		// Permanent: the peer has told us it does not speak our version, and
		// no amount of retrying changes that.
		return http.StatusBadRequest, 0, fmt.Errorf("peer %s does not speak %s", row.PeerDomain, Version)
	}
	if int64(len(row.Body)) > peer.EnvelopeCap() {
		return http.StatusRequestEntityTooLarge, 0, fmt.Errorf(
			"envelope is %d bytes, peer %s accepts %d", len(row.Body), row.PeerDomain, peer.EnvelopeCap())
	}

	nonce, err := NewNonce()
	if err != nil {
		return 0, 0, err
	}
	date := HTTPDate(time.Now())
	signature := Sign(origin.key, Version, origin.Domain, nonce, date, row.Body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		BaseURL(row.PeerDomain)+InboundPath, bytes.NewReader(row.Body))
	if err != nil {
		return 0, 0, fmt.Errorf("build inbound request: %w", err)
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
		return 0, 0, err
	}
	defer resp.Body.Close()
	// The body is small and only used for logging, but it still has to be read
	// for the connection to be reusable.
	detail, _ := readCapped(resp.Body, 4096)

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return http.StatusBadRequest, 0, fmt.Errorf("peer %s redirected inbound, which is not honoured", row.PeerDomain)
	}
	if resp.StatusCode == http.StatusAccepted {
		return resp.StatusCode, 0, nil
	}
	// A signature the peer refuses may just mean it holds a stale key for us.
	// Refreshing our own descriptor is not possible from here, but forcing a
	// re-read of theirs costs one request and fixes the mirror-image case.
	if resp.StatusCode == http.StatusUnauthorized {
		if _, refreshErr := s.Peer(ctx, row.PeerDomain, true); refreshErr != nil {
			s.log.Warn("forced peer refresh failed", "peer", row.PeerDomain, "error", refreshErr)
		}
	}
	return resp.StatusCode, retryAfterOf(resp), fmt.Errorf("peer %s answered %d: %s",
		row.PeerDomain, resp.StatusCode, strings.TrimSpace(detail))
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
		"UPDATE `federation_outbox` SET `status` = ?, `attempts` = `attempts` + 1, "+
			"`last_http_status` = ?, `last_error` = ?, `delivered_at` = NOW() WHERE `id` = ?",
		status, nullableStatus(httpStatus), nullableDetail(detail), row.ID); err != nil {
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
	if _, err := s.db.ExecContext(ctx,
		"UPDATE `federation_outbox` SET `status` = 'pending', `attempts` = `attempts` + 1, "+
			"`last_http_status` = ?, `last_error` = ?, `next_retry_at` = DATE_ADD(NOW(), INTERVAL ? SECOND) "+
			"WHERE `id` = ?",
		nullableStatus(httpStatus), nullableDetail(detail), int(delay.Seconds()), row.ID); err != nil {
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
	statements := []struct {
		query string
		arg   int
	}{
		{"DELETE FROM `federation_inbox` WHERE `received_at` < DATE_SUB(NOW(), INTERVAL ? HOUR)",
			int(DedupRetention.Hours())},
		{"DELETE FROM `federation_nonce` WHERE `seen_at` < DATE_SUB(NOW(), INTERVAL ? MINUTE)",
			int(NonceWindow.Minutes())},
		{"DELETE FROM `federation_outbox` WHERE `status` = 'delivered' AND " +
			"`delivered_at` < DATE_SUB(NOW(), INTERVAL ? HOUR)", int(DedupRetention.Hours())},
	}
	for _, s2 := range statements {
		if _, err := s.db.ExecContext(ctx, s2.query, s2.arg); err != nil {
			return fmt.Errorf("sweep federation state: %w", err)
		}
	}
	return nil
}

// RequeueStuck returns rows left in flight by a worker that died mid-attempt.
// At-least-once delivery plus receiver-side dedup is what makes this safe: the
// peer may have accepted the envelope already and will replay its 202.
func (s *Service) RequeueStuck(ctx context.Context, olderThan time.Duration) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE `federation_outbox` SET `status` = 'pending' WHERE `status` = 'in_flight' "+
			"AND `next_retry_at` < DATE_SUB(NOW(), INTERVAL ? SECOND)", int(olderThan.Seconds()))
	if err != nil {
		return fmt.Errorf("requeue stuck envelopes: %w", err)
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
	if len(detail) > 2000 {
		detail = detail[:2000]
	}
	return detail
}
