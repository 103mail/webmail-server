package email

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/mdconv"
	"github.com/103mail/webmail-server/internal/store"
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
// It returns how many recipients were delivered to, and the local recipients
// that were not because they have no account here. None delivered is answered
// 404 and some refused are listed in the 202 (RFC 6.3, 6.5). Incoming mail
// never creates an account. A repeat of the same message writes nothing and
// returns the same answer.
func (s *Service) DeliverInbound(ctx context.Context, in federation.Inbound) (int, []string, error) {
	in.SenderEmail = store.Address(in.SenderEmail)
	recipients := make([]string, len(in.Recipients))
	for i, address := range in.Recipients {
		recipients[i] = store.Address(address)
	}
	in.Recipients = recipients
	mine := s.localRecipients(in)
	if len(mine) == 0 {
		return 0, nil, nil
	}

	var refused []string
	sender := identity{Email: in.SenderEmail, Name: in.SenderName}

	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		gThreadID, known, err := s.inboundThread(ctx, tx, in)
		if err != nil {
			return err
		}
		// A recipient who left is not delivered to, even by an envelope
		// written before their notice reached its sender, and is not refused
		// either: they exist, they left (RFC 5.6).
		if mine, err = s.withoutDeparted(ctx, tx, gThreadID, mine); err != nil {
			return err
		}

		// Idempotency below the protocol's own dedup: if this exact message is
		// already here, a retry that raced the dedup record still cannot
		// duplicate it.
		existing, err := store.SelectInt(ctx, tx,
			`SELECT "id" FROM "g_message" WHERE "federation_id" = ? LIMIT 1`, in.MessageFedID)
		if err != nil {
			return fmt.Errorf("look up federated message: %w", err)
		}
		if existing != 0 {
			refused, err = s.undelivered(ctx, tx, in.MessageFedID, mine)
			return err
		}
		// A thread held here is written to only by its members. A peer that
		// names itself a member in thread.members does not become one: who is
		// on a thread grows only by a member adding someone (RFC 5.6).
		if known {
			if err := s.checkInboundMember(ctx, tx, gThreadID, in); err != nil {
				return err
			}
		}

		body := in.Body
		if body == "" && in.BodyHTML != "" {
			body = mdconv.ToMarkdown(in.BodyHTML)
		}
		summary := mdconv.Summary(body)
		if _, err := tx.ExecContext(ctx,
			`UPDATE "g_thread" SET "body_summary" = ?, "updated_at" = ? WHERE "id" = ?`,
			summary, in.SentAt, gThreadID); err != nil {
			return fmt.Errorf("update thread summary: %w", err)
		}

		gMessageID, err := store.Insert(ctx, tx,
			`INSERT INTO "g_message" ("g_thread_id", "sender_email", "sender_name", "body", `+
				`"body_html", "created_at", "sent_at", "federation_id") VALUES (?,?,?,?,?,?,?,?)`,
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
		// including the subscribers who live on other Domains. The sender is
		// normally also in the recipient list.
		members := in.Members
		if len(members) == 0 {
			members = append([]string{in.SenderEmail}, in.Recipients...)
		}
		members, err = s.withoutUnknown(ctx, tx, members)
		if err != nil {
			return err
		}
		if err := s.subscribeAll(ctx, tx, gThreadID, members); err != nil {
			return err
		}
		if err := s.applyRefusals(ctx, tx, gThreadID, in); err != nil {
			return err
		}

		for _, address := range mine {
			if _, err := s.projectInbound(ctx, tx, gThreadID, gMessageID, gAttachmentIDs, address, sender); err != nil {
				return err
			}
		}
		refused, err = s.undelivered(ctx, tx, in.MessageFedID, mine)
		if err != nil || len(refused) == 0 {
			return err
		}
		return s.tellRefusal(ctx, tx, gThreadID, refused)
	})
	if err != nil {
		return 0, nil, err
	}
	return len(mine) - len(refused), refused, nil
}

// Undelivered lists the local recipients of a federated message that have no
// copy of it. It only reads, so a replayed envelope is answered with the list
// its first delivery produced (RFC 5.4).
func (s *Service) Undelivered(ctx context.Context, in federation.Inbound) ([]string, error) {
	gThreadID, err := store.SelectInt(ctx, s.db,
		`SELECT "g_thread_id" FROM "g_message" WHERE "federation_id" = ? LIMIT 1`, in.MessageFedID)
	if err != nil {
		return nil, fmt.Errorf("look up federated message: %w", err)
	}
	mine, err := s.withoutDeparted(ctx, s.db, gThreadID, s.localRecipients(in))
	if err != nil {
		return nil, err
	}
	return s.undelivered(ctx, s.db, in.MessageFedID, mine)
}

// withoutDeparted drops the addresses that left the thread.
func (s *Service) withoutDeparted(ctx context.Context, q store.Queryer, gThreadID int64,
	addresses []string) ([]string, error) {
	departed, err := store.SelectStrings(ctx, q,
		`SELECT "email" FROM "g_subscription" WHERE "g_thread_id" = ? AND "left_at" IS NOT NULL`, gThreadID)
	if err != nil {
		return nil, fmt.Errorf("list departed members: %w", err)
	}
	return slices.DeleteFunc(slices.Clone(addresses), func(a string) bool { return slices.Contains(departed, a) }), nil
}

// localRecipients are the envelope's recipients on a Domain this Server is
// authoritative for, lowercase. The envelope names the whole subscriber set
// so the thread's membership is complete, but a Domain delivers to its own
// recipients and no one else's.
func (s *Service) localRecipients(in federation.Inbound) []string {
	var mine []string
	for _, address := range in.Recipients {
		if address = store.Address(address); s.isLocal(address) && !slices.Contains(mine, address) {
			mine = append(mine, address)
		}
	}
	return mine
}

// undelivered returns the addresses with no copy of a federated message.
func (s *Service) undelivered(ctx context.Context, q store.Queryer, messageFedID string,
	addresses []string) ([]string, error) {
	var out []string
	for _, address := range addresses {
		n, err := store.SelectInt(ctx, q,
			`SELECT COUNT(*) FROM "message" `+
				`JOIN "g_message" ON "g_message"."id" = "message"."g_message_id" `+
				`JOIN "alias" ON "alias"."customer_id" = "message"."customer_id" `+
				`WHERE "g_message"."federation_id" = ? AND "alias"."email" = ?`,
			messageFedID, address)
		if err != nil {
			return nil, fmt.Errorf("look up delivery to %s: %w", address, err)
		}
		if n == 0 {
			out = append(out, address)
		}
	}
	return out, nil
}

// applyRefusals stops treating the addresses a message refuses or reports as
// departed as members, but only those on the sender's own domain: a Domain
// speaks for its own addresses and no others (RFC 5.3, 5.6, 6.5).
func (s *Service) applyRefusals(ctx context.Context, tx *sql.Tx, gThreadID int64, in federation.Inbound) error {
	origin := federation.DomainOf(in.SenderEmail)
	for column, addresses := range map[string][]string{"refused_at": in.Refused, "left_at": in.Left} {
		args := []any{s.db.Now(), gThreadID}
		for _, address := range addresses {
			if address = store.Address(address); federation.DomainOf(address) == origin {
				args = append(args, address)
			}
		}
		if len(args) == 2 {
			continue
		}
		// column is one of two literals above, not input.
		if _, err := tx.ExecContext(ctx,
			`UPDATE "g_subscription" SET "`+column+`" = ? WHERE "g_thread_id" = ? AND `+isMember+` `+
				`AND "email" IN (`+store.Placeholders(len(args)-2)+`)`, args...); err != nil {
			return fmt.Errorf("apply %s: %w", column, err)
		}
	}
	return nil
}

// tellRefusal tells the thread that addresses on this Domain do not exist,
// the way an invitation is told: a message in the thread, here from this
// Domain's mailer-daemon, shown to the members on this Server and sent to
// every other member's Domain with the addresses refused (RFC 6.5).
func (s *Service) tellRefusal(ctx context.Context, tx *sql.Tx, gThreadID int64, refused []string) error {
	domain := federation.DomainOf(refused[0])
	sender := identity{Email: "mailer-daemon@" + domain, Name: "Mailer Daemon"}
	body := fmt.Sprintf("%s does not exist on %s, so it is not on this thread.",
		strings.Join(refused, ", "), domain)
	now := s.db.Now()
	fedID, err := federation.NewUUID()
	if err != nil {
		return err
	}
	gMessageID, err := store.Insert(ctx, tx,
		`INSERT INTO "g_message" ("g_thread_id", "sender_email", "sender_name", "body", `+
			`"created_at", "sent_at", "federation_id") VALUES (?,?,?,?,?,?,?)`,
		gThreadID, sender.Email, sender.Name, body, now, now, fedID)
	if err != nil {
		return fmt.Errorf("create refusal notice: %w", err)
	}

	members, err := store.SelectStrings(ctx, tx,
		`SELECT "email" FROM "g_subscription" WHERE "g_thread_id" = ? AND `+isMember, gThreadID)
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}
	remote := map[string][]string{}
	for _, member := range members {
		if federation.IsMailerDaemon(member) {
			continue
		}
		if s.isLocal(member) {
			if _, err := s.projectInbound(ctx, tx, gThreadID, gMessageID, nil, member, sender); err != nil {
				return err
			}
			continue
		}
		remote[federation.DomainOf(member)] = append(remote[federation.DomainOf(member)], member)
	}
	if len(remote) == 0 || s.federation == nil {
		return nil
	}
	return s.federate(ctx, tx, gThreadID, gMessageID, now, sender, members, refused, nil, remote, body, "", nil)
}

// withoutUnknown drops addresses on this Server's own Domains that have no
// account. They were refused, so they are not members (RFC 5.3).
func (s *Service) withoutUnknown(ctx context.Context, q store.Queryer, addresses []string) ([]string, error) {
	out := make([]string, 0, len(addresses))
	for _, address := range addresses {
		address = store.Address(address)
		own, err := s.ownDomain(ctx, q, address)
		if err != nil {
			return nil, err
		}
		if own {
			known, err := store.SelectInt(ctx, q, `SELECT COUNT(*) FROM "alias" WHERE "email" = ?`, address)
			if err != nil {
				return nil, fmt.Errorf("look up %s: %w", address, err)
			}
			if known == 0 {
				continue
			}
		}
		out = append(out, address)
	}
	return out, nil
}

// subscribeAll adds addresses to a thread's subscriber set, skipping the ones
// already there. Addresses are lowercase, so the unique index on
// (g_thread_id, email) is what does the skipping.
func (s *Service) subscribeAll(ctx context.Context, tx *sql.Tx, gThreadID int64, addresses []string) error {
	for _, address := range addresses {
		if address == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "g_subscription" ("g_thread_id", "email") VALUES (?,?) ON CONFLICT DO NOTHING`,
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
func (s *Service) inboundThread(ctx context.Context, tx *sql.Tx, in federation.Inbound) (int64, bool, error) {
	for _, candidate := range []string{in.ThreadFedID, in.InReplyTo} {
		if candidate == "" {
			continue
		}
		id, err := store.SelectInt(ctx, tx,
			`SELECT "id" FROM "g_thread" WHERE "federation_id" = ? LIMIT 1`, candidate)
		if err != nil {
			return 0, false, fmt.Errorf("look up federated thread: %w", err)
		}
		if id != 0 {
			return id, true, nil
		}
	}
	id, err := store.Insert(ctx, tx,
		`INSERT INTO "g_thread" ("subject_line", "created_at", "updated_at", "federation_id") `+
			"VALUES (?,?,?,?)", in.Subject, in.SentAt, in.SentAt, in.ThreadFedID)
	if err != nil {
		return 0, false, fmt.Errorf("create federated thread: %w", err)
	}
	return id, false, nil
}

// checkInboundMember refuses a message on a thread held here from someone who
// is not a member of it. A Domain's mailer-daemon, which tells the thread
// about refusals, is never a member and is let through.
func (s *Service) checkInboundMember(ctx context.Context, tx *sql.Tx, gThreadID int64, in federation.Inbound) error {
	if federation.IsMailerDaemon(in.SenderEmail) {
		return nil
	}
	n, err := store.SelectInt(ctx, tx,
		`SELECT COUNT(*) FROM "g_subscription" WHERE "g_thread_id" = ? AND "email" = ? AND `+isMember,
		gThreadID, in.SenderEmail)
	if err != nil {
		return fmt.Errorf("look up sender membership: %w", err)
	}
	if n == 0 {
		return &NotMemberError{Address: in.SenderEmail}
	}
	return nil
}

// projectInbound writes one recipient's view of a federated message. It reports
// false when the address has no mailbox, which means onboarding did not
// complete and there is nowhere to put it.
func (s *Service) projectInbound(ctx context.Context, tx *sql.Tx, gThreadID, gMessageID int64,
	gAttachmentIDs []int64, address string, sender identity) (bool, error) {
	mailboxID, err := store.SelectInt(ctx, tx,
		`SELECT "mailbox_id" FROM "alias" WHERE "email" = ? LIMIT 1`, address)
	if err != nil {
		return false, fmt.Errorf("look up recipient mailbox: %w", err)
	}
	if mailboxID == 0 {
		return false, nil
	}
	folderID, err := store.SelectInt(ctx, tx,
		`SELECT "default_folder_id" FROM "mailbox" WHERE "id" = ? LIMIT 1`, mailboxID)
	if err != nil {
		return false, fmt.Errorf("look up recipient folder: %w", err)
	}
	customerID, err := store.SelectInt(ctx, tx,
		`SELECT "customer_id" FROM "folder" WHERE "id" = ? LIMIT 1`, folderID)
	if err != nil {
		return false, fmt.Errorf("look up recipient customer: %w", err)
	}
	if folderID == 0 || customerID == 0 {
		return false, nil
	}

	threadID, err := store.SelectInt(ctx, tx,
		`SELECT "id" FROM "thread" WHERE "customer_id" = ? AND "g_thread_id" = ? LIMIT 1`,
		customerID, gThreadID)
	if err != nil {
		return false, fmt.Errorf("look up recipient thread: %w", err)
	}
	if threadID == 0 {
		threadID, err = store.Insert(ctx, tx,
			`INSERT INTO "thread" ("customer_id", "folder_id", "g_thread_id") VALUES (?,?,?)`,
			customerID, folderID, gThreadID)
		if err != nil {
			return false, fmt.Errorf("create recipient thread: %w", err)
		}
		if err := s.projectHistory(ctx, tx, customerID, threadID, gThreadID, gMessageID); err != nil {
			return false, err
		}
	}
	projectionID, err := store.Insert(ctx, tx,
		`INSERT INTO "message" ("customer_id", "thread_id", "g_message_id") VALUES (?,?,?)`,
		customerID, threadID, gMessageID)
	if err != nil {
		return false, fmt.Errorf("create recipient message: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE "thread" SET "read_at" = NULL WHERE "id" = ?`, threadID); err != nil {
		return false, fmt.Errorf("mark recipient thread unread: %w", err)
	}
	for _, gAttachmentID := range gAttachmentIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "attachment" ("customer_id", "message_id", "g_attachment_id") VALUES (?,?,?)`,
			customerID, projectionID, gAttachmentID); err != nil {
			return false, fmt.Errorf("create recipient attachment: %w", err)
		}
	}
	// A mailer-daemon wrote a notice, it is not someone to write back to.
	if !federation.IsMailerDaemon(sender.Email) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "contact" ("customer_id", "email", "name") VALUES (?,?,?) ON CONFLICT DO NOTHING`,
			customerID, sender.Email, sender.Name); err != nil {
			return false, fmt.Errorf("record sender as contact: %w", err)
		}
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
			`INSERT INTO "g_attachment" ("g_message_id", "file_name", "mime_type", "file_size", `+
				`"hash_md5", "federation_id") VALUES (?,?,?,?,?,?)`,
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
	return truncate(name, 255)
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
	if b.Backfill {
		body += fmt.Sprintf("\nIt will be sent again when %s can be reached.\n", b.PeerDomain)
	}

	senderEmail := store.Address(b.SenderEmail)
	now := s.db.Now()
	bounceFedID, err := federation.NewUUID()
	if err != nil {
		return err
	}
	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		mailboxID, err := store.SelectInt(ctx, tx,
			`SELECT "mailbox_id" FROM "alias" WHERE "email" = ? LIMIT 1`, senderEmail)
		if err != nil {
			return fmt.Errorf("look up bounce recipient: %w", err)
		}
		if mailboxID == 0 {
			return fmt.Errorf("cannot bounce to %s: no mailbox", b.SenderEmail)
		}
		folderID, err := store.SelectInt(ctx, tx,
			`SELECT "default_folder_id" FROM "mailbox" WHERE "id" = ? LIMIT 1`, mailboxID)
		if err != nil {
			return fmt.Errorf("look up bounce folder: %w", err)
		}
		customerID, err := store.SelectInt(ctx, tx,
			`SELECT "customer_id" FROM "folder" WHERE "id" = ? LIMIT 1`, folderID)
		if err != nil {
			return fmt.Errorf("look up bounce customer: %w", err)
		}
		if folderID == 0 || customerID == 0 {
			return fmt.Errorf("cannot bounce to %s: no folder", b.SenderEmail)
		}

		gThreadID, err := store.Insert(ctx, tx,
			`INSERT INTO "g_thread" ("subject_line", "body_summary") VALUES (?,?)`,
			truncate("Delivery failed: "+b.Subject, 255), mdconv.Summary(body))
		if err != nil {
			return fmt.Errorf("create bounce thread: %w", err)
		}
		gMessageID, err := store.Insert(ctx, tx,
			`INSERT INTO "g_message" ("g_thread_id", "sender_email", "sender_name", "body", `+
				`"created_at", "sent_at", "federation_id") VALUES (?,?,?,?,?,?,?)`,
			gThreadID, "mailer-daemon@"+b.OriginDomain, "Mailer Daemon", body, now, now, bounceFedID)
		if err != nil {
			return fmt.Errorf("create bounce message: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "g_subscription" ("g_thread_id", "email") VALUES (?,?) ON CONFLICT DO NOTHING`,
			gThreadID, senderEmail); err != nil {
			return fmt.Errorf("subscribe bounce recipient: %w", err)
		}
		threadID, err := store.Insert(ctx, tx,
			`INSERT INTO "thread" ("customer_id", "folder_id", "g_thread_id") VALUES (?,?,?)`,
			customerID, folderID, gThreadID)
		if err != nil {
			return fmt.Errorf("create bounce thread projection: %w", err)
		}
		if _, err := store.Insert(ctx, tx,
			`INSERT INTO "message" ("customer_id", "thread_id", "g_message_id") VALUES (?,?,?)`,
			customerID, threadID, gMessageID); err != nil {
			return fmt.Errorf("create bounce message projection: %w", err)
		}

		// Recipients that do not exist did not receive the message and are not
		// members: marked, not deleted, so later messages and the member list
		// leave them out and the sender is told once (RFC 6.5).
		if b.Refused && b.ThreadFedID != "" && len(b.Recipients) > 0 {
			args := []any{now, b.ThreadFedID}
			for _, r := range b.Recipients {
				args = append(args, store.Address(r))
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE "g_subscription" SET "refused_at" = ? WHERE "refused_at" IS NULL `+
					`AND "g_thread_id" IN (SELECT "id" FROM "g_thread" WHERE "federation_id" = ?) `+
					`AND "email" IN (`+store.Placeholders(len(b.Recipients))+`)`,
				args...); err != nil {
				return fmt.Errorf("mark refused recipients: %w", err)
			}
		}
		return nil
	})
}

// truncate shortens v to at most n characters. It counts runes, not bytes,
// both because varchar(n) does and because cutting inside a UTF-8 sequence
// leaves text PostgreSQL refuses to store.
func truncate(v string, n int) string {
	r := []rune(v)
	if len(r) <= n {
		return v
	}
	return string(r[:n])
}
