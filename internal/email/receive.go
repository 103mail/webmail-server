package email

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/mevdschee/webmail-server/internal/federation"
	"github.com/mevdschee/webmail-server/internal/mdconv"
	"github.com/mevdschee/webmail-server/internal/store"
)

// DeliverInbound writes an envelope accepted from a peer, satisfying
// federation.Deliverer.
//
// The rows it writes are the same rows a local send writes: one canonical
// g_thread, g_message and g_attachment set, then one projection per recipient.
// A federated message is not a second class of message; the only difference is
// that the canonical rows carry the identifiers the origin chose, so the two
// Domains agree on what this thread is (ADR 17).
//
// It returns the number of recipients actually delivered to. Zero means none of
// them are known here and none were onboarded, which the caller answers 404.
func (s *Service) DeliverInbound(ctx context.Context, in federation.Inbound) (int, error) {
	// Only the addresses this Server is authoritative for. The envelope names
	// the whole subscriber set so the thread's membership is complete, but a
	// Domain delivers to its own recipients and no one else's.
	var mine []string
	for _, address := range in.Recipients {
		if s.isLocal(address) {
			mine = append(mine, address)
		}
	}
	if len(mine) == 0 {
		return 0, nil
	}

	var delivered int
	var onboarded []invitee
	sender := identity{Email: in.SenderEmail, Name: in.SenderName}

	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		gThreadID, err := s.inboundThread(ctx, tx, in)
		if err != nil {
			return err
		}

		// Idempotency below the protocol's own dedup: if this exact message is
		// already here, a retry that raced the dedup record still cannot
		// duplicate it.
		existing, err := store.SelectInt(ctx, tx,
			"SELECT `id` FROM `g_message` WHERE `federation_id` = ? LIMIT 1", in.MessageFedID)
		if err != nil {
			return fmt.Errorf("look up federated message: %w", err)
		}
		if existing != 0 {
			delivered = len(mine)
			return nil
		}

		body := in.Body
		if body == "" && in.BodyHTML != "" {
			body = mdconv.ToMarkdown(in.BodyHTML)
		}
		summary := mdconv.Summary(body)
		if _, err := tx.ExecContext(ctx,
			"UPDATE `g_thread` SET `body_summary` = ?, `updated_at` = ? WHERE `id` = ? LIMIT 1",
			summary, in.SentAt, gThreadID); err != nil {
			return fmt.Errorf("update thread summary: %w", err)
		}

		gMessageID, err := store.Insert(ctx, tx,
			"INSERT INTO `g_message` (`g_thread_id`, `sender_email`, `sender_name`, `body`, "+
				"`body_html`, `created_at`, `sent_at`, `federation_id`) VALUES (?,?,?,?,?,?,?,?)",
			gThreadID, in.SenderEmail, in.SenderName, body, in.BodyHTML,
			in.SentAt, in.SentAt, in.MessageFedID)
		if err != nil {
			return fmt.Errorf("create federated message: %w", err)
		}

		gAttachmentIDs, err := s.storeInboundAttachments(ctx, tx, gMessageID, in.Attachments)
		if err != nil {
			return err
		}

		// The subscriber set is the whole envelope's recipient list plus the
		// sender, so that a reply from here reaches everyone on the thread,
		// including the subscribers who live on other Domains.
		//
		// Deduplicated here rather than left to INSERT IGNORE: g_subscription
		// carries no unique index on (g_thread_id, email), so IGNORE only
		// suppresses errors, it does not suppress duplicate rows. The sender is
		// normally also in the recipient list, so this would double them.
		if err := s.subscribeAll(ctx, tx, gThreadID,
			append([]string{in.SenderEmail}, in.Recipients...)); err != nil {
			return err
		}

		for _, address := range mine {
			// A local recipient with no account is onboarded here, on their own
			// Domain, which is the one place it is correct to do so: this is
			// the Domain their address actually belongs to (design 8).
			if err := s.ensureCustomer(ctx, tx, address, "", &onboarded); err != nil {
				return err
			}
			ok, err := s.projectInbound(ctx, tx, gThreadID, gMessageID, gAttachmentIDs, address, sender)
			if err != nil {
				return err
			}
			if ok {
				delivered++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.invite(ctx, sender, in.Subject, onboarded)
	return delivered, nil
}

// subscribeAll adds addresses to a thread's subscriber set, skipping the ones
// already there. It reads the existing set first because the table has no
// unique index to lean on.
func (s *Service) subscribeAll(ctx context.Context, tx *sql.Tx, gThreadID int64, addresses []string) error {
	existing, err := store.SelectStrings(ctx, tx,
		"SELECT `email` FROM `g_subscription` WHERE `g_thread_id` = ?", gThreadID)
	if err != nil {
		return fmt.Errorf("list subscribers: %w", err)
	}
	seen := make(map[string]bool, len(existing)+len(addresses))
	for _, address := range existing {
		seen[strings.ToLower(address)] = true
	}
	for _, address := range addresses {
		key := strings.ToLower(address)
		if address == "" || seen[key] {
			continue
		}
		seen[key] = true
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `g_subscription` (`g_thread_id`, `email`) VALUES (?,?)",
			gThreadID, address); err != nil {
			return fmt.Errorf("subscribe %s: %w", address, err)
		}
	}
	return nil
}

// inboundThread finds the thread an envelope belongs to, or creates it.
//
// Correlation runs on federated identifiers rather than subject lines: the
// thread's own id if we have seen it, then in_reply_to. A peer may legitimately
// send an in_reply_to we have never seen, in which case a fresh thread is the
// specified behaviour rather than an error (RFC 5.3).
func (s *Service) inboundThread(ctx context.Context, tx *sql.Tx, in federation.Inbound) (int64, error) {
	for _, candidate := range []string{in.ThreadFedID, in.InReplyTo} {
		if candidate == "" {
			continue
		}
		id, err := store.SelectInt(ctx, tx,
			"SELECT `id` FROM `g_thread` WHERE `federation_id` = ? LIMIT 1", candidate)
		if err != nil {
			return 0, fmt.Errorf("look up federated thread: %w", err)
		}
		if id != 0 {
			return id, nil
		}
	}
	id, err := store.Insert(ctx, tx,
		"INSERT INTO `g_thread` (`subject_line`, `created_at`, `updated_at`, `federation_id`) "+
			"VALUES (?,?,?,?)", in.Subject, in.SentAt, in.SentAt, in.ThreadFedID)
	if err != nil {
		return 0, fmt.Errorf("create federated thread: %w", err)
	}
	return id, nil
}

// projectInbound writes one recipient's view of a federated message. It reports
// false when the address has no mailbox, which means onboarding did not
// complete and there is nowhere to put it.
func (s *Service) projectInbound(ctx context.Context, tx *sql.Tx, gThreadID, gMessageID int64,
	gAttachmentIDs []int64, address string, sender identity) (bool, error) {
	mailboxID, err := store.SelectInt(ctx, tx,
		"SELECT `mailbox_id` FROM `alias` WHERE `email` = ? LIMIT 1", address)
	if err != nil {
		return false, fmt.Errorf("look up recipient mailbox: %w", err)
	}
	if mailboxID == 0 {
		return false, nil
	}
	folderID, err := store.SelectInt(ctx, tx,
		"SELECT `default_folder_id` FROM `mailbox` WHERE `id` = ? LIMIT 1", mailboxID)
	if err != nil {
		return false, fmt.Errorf("look up recipient folder: %w", err)
	}
	customerID, err := store.SelectInt(ctx, tx,
		"SELECT `customer_id` FROM `folder` WHERE `id` = ? LIMIT 1", folderID)
	if err != nil {
		return false, fmt.Errorf("look up recipient customer: %w", err)
	}
	if folderID == 0 || customerID == 0 {
		return false, nil
	}

	threadID, err := store.SelectInt(ctx, tx,
		"SELECT `id` FROM `thread` WHERE `customer_id` = ? AND `g_thread_id` = ? LIMIT 1",
		customerID, gThreadID)
	if err != nil {
		return false, fmt.Errorf("look up recipient thread: %w", err)
	}
	if threadID == 0 {
		threadID, err = store.Insert(ctx, tx,
			"INSERT INTO `thread` (`customer_id`, `folder_id`, `g_thread_id`) VALUES (?,?,?)",
			customerID, folderID, gThreadID)
		if err != nil {
			return false, fmt.Errorf("create recipient thread: %w", err)
		}
	}
	projectionID, err := store.Insert(ctx, tx,
		"INSERT INTO `message` (`customer_id`, `thread_id`, `g_message_id`) VALUES (?,?,?)",
		customerID, threadID, gMessageID)
	if err != nil {
		return false, fmt.Errorf("create recipient message: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE `thread` SET `read_at` = NULL WHERE `id` = ?", threadID); err != nil {
		return false, fmt.Errorf("mark recipient thread unread: %w", err)
	}
	for _, gAttachmentID := range gAttachmentIDs {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `attachment` (`customer_id`, `message_id`, `g_attachment_id`) VALUES (?,?,?)",
			customerID, projectionID, gAttachmentID); err != nil {
			return false, fmt.Errorf("create recipient attachment: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT IGNORE INTO `contact` (`customer_id`, `email`, `name`) VALUES (?,?,?)",
		customerID, sender.Email, sender.Name); err != nil {
		return false, fmt.Errorf("record sender as contact: %w", err)
	}
	return true, nil
}

// storeInboundAttachments writes the bytes that arrived inline. They were
// already checked against the declared size and hash before this is reached, so
// what lands in the blob store is what the origin sent.
func (s *Service) storeInboundAttachments(ctx context.Context, tx *sql.Tx, gMessageID int64,
	attachments []federation.InboundAttachment) ([]int64, error) {
	var ids []int64
	for _, a := range attachments {
		hash, err := s.blob.Put(a.Data)
		if err != nil {
			return nil, err
		}
		id, err := store.Insert(ctx, tx,
			"INSERT INTO `g_attachment` (`g_message_id`, `file_name`, `mime_type`, `file_size`, "+
				"`hash_md5`, `federation_id`) VALUES (?,?,?,?,?,?)",
			gMessageID, sanitizeFileName(a.FileName), a.MimeType, len(a.Data), hash, a.FederationID)
		if err != nil {
			return nil, fmt.Errorf("create federated attachment: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// sanitizeFileName strips path separators and control characters from a name a
// peer chose. RFC 7 requires receivers to sanitize; the name is displayed and
// offered as a download, so a peer must not be able to steer either.
func sanitizeFileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	if len(name) > 255 {
		name = name[:255]
	}
	return name
}

// Bounce writes a delivery failure into the sender's own mailbox, satisfying
// federation.Deliverer.
//
// It is a local message and never leaves this Server. Without it a permanent
// failure would be silent, and the sender would go on believing a message
// arrived because their Sent copy exists (RFC 6.5).
func (s *Service) Bounce(ctx context.Context, b federation.BounceNotice) error {
	reason := b.ErrorCode
	if b.HTTPStatus != 0 {
		reason = fmt.Sprintf("%s (HTTP %d)", b.ErrorCode, b.HTTPStatus)
	}
	body := fmt.Sprintf(
		"Your message could not be delivered.\n\n"+
			"Recipients: %s\nDestination domain: %s\nReason: %s\nEnvelope: %s\n",
		strings.Join(b.Recipients, ", "), b.PeerDomain, reason, b.EnvelopeID)

	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		mailboxID, err := store.SelectInt(ctx, tx,
			"SELECT `mailbox_id` FROM `alias` WHERE `email` = ? LIMIT 1", b.SenderEmail)
		if err != nil {
			return fmt.Errorf("look up bounce recipient: %w", err)
		}
		if mailboxID == 0 {
			return fmt.Errorf("cannot bounce to %s: no mailbox", b.SenderEmail)
		}
		folderID, err := store.SelectInt(ctx, tx,
			"SELECT `default_folder_id` FROM `mailbox` WHERE `id` = ? LIMIT 1", mailboxID)
		if err != nil {
			return fmt.Errorf("look up bounce folder: %w", err)
		}
		customerID, err := store.SelectInt(ctx, tx,
			"SELECT `customer_id` FROM `folder` WHERE `id` = ? LIMIT 1", folderID)
		if err != nil {
			return fmt.Errorf("look up bounce customer: %w", err)
		}
		if folderID == 0 || customerID == 0 {
			return fmt.Errorf("cannot bounce to %s: no folder", b.SenderEmail)
		}

		gThreadID, err := store.Insert(ctx, tx,
			"INSERT INTO `g_thread` (`subject_line`, `body_summary`) VALUES (?,?)",
			truncate("Delivery failed: "+b.Subject, 255), mdconv.Summary(body))
		if err != nil {
			return fmt.Errorf("create bounce thread: %w", err)
		}
		gMessageID, err := store.Insert(ctx, tx,
			"INSERT INTO `g_message` (`g_thread_id`, `sender_email`, `sender_name`, `body`, "+
				"`created_at`, `sent_at`) VALUES (?,?,?,?,NOW(),NOW())",
			gThreadID, "mailer-daemon@"+b.OriginDomain, "Mailer Daemon", body)
		if err != nil {
			return fmt.Errorf("create bounce message: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT IGNORE INTO `g_subscription` (`g_thread_id`, `email`) VALUES (?,?)",
			gThreadID, b.SenderEmail); err != nil {
			return fmt.Errorf("subscribe bounce recipient: %w", err)
		}
		threadID, err := store.Insert(ctx, tx,
			"INSERT INTO `thread` (`customer_id`, `folder_id`, `g_thread_id`) VALUES (?,?,?)",
			customerID, folderID, gThreadID)
		if err != nil {
			return fmt.Errorf("create bounce thread projection: %w", err)
		}
		if _, err := store.Insert(ctx, tx,
			"INSERT INTO `message` (`customer_id`, `thread_id`, `g_message_id`) VALUES (?,?,?)",
			customerID, threadID, gMessageID); err != nil {
			return fmt.Errorf("create bounce message projection: %w", err)
		}
		return nil
	})
}

func truncate(v string, n int) string {
	if len(v) <= n {
		return v
	}
	return v[:n]
}
