package chat

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/store"
)

// DeliverChat writes a chat message accepted from another Domain, satisfying
// federation.ChatDeliverer.
//
// The rows are the ones a local send writes, with the identifiers and the
// send time the Origin chose, so every Domain holds the same chat in the same
// order (RFC 14.3, 14.4). A chat this Server does not hold yet is created for
// its recipients, and a message it already holds is not written twice, which
// is what lets history arrive from several Domains at once (RFC 14.5).
func (s *Service) DeliverChat(ctx context.Context, in federation.ChatInbound) (int, []string, error) {
	var delivered int
	var refused []string
	var gChatID, gMessageID int64
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		delivered, refused = 0, nil
		var created bool
		var err error
		gChatID, created, err = s.inboundChat(ctx, tx, in.ChatFedID, in.Type, in.Name, in.Members, in.SenderEmail, true)
		if err != nil {
			return err
		}
		if !created {
			if err := checkSender(ctx, tx, gChatID, in); err != nil {
				return err
			}
		}
		added, err := s.applyMembership(ctx, tx, gChatID, in.OriginDomain, in.Members, append(in.Left, in.Refused...))
		if err != nil {
			return err
		}
		now := s.db.Now()
		// Someone added here sees what this Server already holds of the chat,
		// as when they are added locally (ADR 9).
		for _, address := range added {
			if s.remote(address) {
				continue
			}
			if err := projectChat(ctx, tx, gChatID, address); err != nil {
				return err
			}
			if err := projectHistory(ctx, tx, gChatID, address, now); err != nil {
				return err
			}
		}

		gMessageID, err = s.inboundMessage(ctx, tx, gChatID, in)
		if err != nil {
			return err
		}
		active, err := activeParticipants(ctx, tx, gChatID)
		if err != nil {
			return err
		}
		for _, address := range in.Recipients {
			hasAccount, err := store.SelectInt(ctx, tx,
				`SELECT COUNT(*) FROM "alias" WHERE "email" = ?`, address)
			if err != nil {
				return fmt.Errorf("look up recipient: %w", err)
			}
			if hasAccount == 0 {
				refused = append(refused, address)
				continue
			}
			// Whoever left gets nothing new, even from an envelope written
			// before their notice reached its sender (RFC 5.6, 14.5).
			if !contains(active, address) {
				continue
			}
			if err := projectChat(ctx, tx, gChatID, address); err != nil {
				return err
			}
			if err := s.projectInbound(ctx, tx, gChatID, gMessageID, address, in.History, now); err != nil {
				return err
			}
			delivered++
		}
		// Members added by this message, wherever they are, get the earlier
		// messages of this Server's own people (RFC 14.5).
		if s.fed != nil {
			return s.sendHistory(ctx, tx, gChatID, added, gMessageID)
		}
		return nil
	})
	if err == nil && delivered > 0 && !in.History && s.received != nil {
		s.received(ctx, gChatID, gMessageID)
	}
	return delivered, refused, err
}

// UndeliveredChat lists the recipients of a chat envelope that have no account
// here, which is what its first delivery refused (RFC 5.4).
func (s *Service) UndeliveredChat(ctx context.Context, in federation.ChatInbound) ([]string, error) {
	var refused []string
	for _, address := range in.Recipients {
		n, err := store.SelectInt(ctx, s.db, `SELECT COUNT(*) FROM "alias" WHERE "email" = ?`, address)
		if err != nil {
			return nil, fmt.Errorf("look up recipient: %w", err)
		}
		if n == 0 {
			refused = append(refused, address)
		}
	}
	return refused, nil
}

// inboundChat finds the chat an envelope is about: by its federation id, or,
// for a direct chat, by its two members whatever id it was given. Two people
// who started one at the same moment end up in one chat, under the lower of
// the two ids (RFC 14.3). With create, a chat not held here is created.
func (s *Service) inboundChat(ctx context.Context, tx *sql.Tx, fedID, chatType string, name *string,
	members []string, creator string, create bool) (int64, bool, error) {
	gChatID, err := store.SelectInt(ctx, tx,
		`SELECT "id" FROM "g_chat" WHERE "federation_id" = ? LIMIT 1`, fedID)
	if err != nil {
		return 0, false, fmt.Errorf("look up chat: %w", err)
	}
	if gChatID != 0 {
		return gChatID, false, nil
	}
	if chatType == "direct" && len(members) == 2 {
		var existing sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT "g_chat"."id", "g_chat"."federation_id" FROM "g_chat" WHERE "g_chat"."type" = 'direct' `+
				`AND (SELECT COUNT(DISTINCT "email") FROM "g_chat_participant" "p" `+
				`WHERE "p"."g_chat_id" = "g_chat"."id" AND "p"."email" IN (?,?)) = 2 `+
				`AND NOT EXISTS (SELECT 1 FROM "g_chat_participant" "p" `+
				`WHERE "p"."g_chat_id" = "g_chat"."id" AND "p"."email" NOT IN (?,?)) `+
				`ORDER BY "g_chat"."id" LIMIT 1`,
			members[0], members[1], members[0], members[1]).Scan(&gChatID, &existing)
		if err != nil && err != sql.ErrNoRows {
			return 0, false, fmt.Errorf("look up direct chat: %w", err)
		}
		if gChatID != 0 {
			if !existing.Valid || existing.String == "" || strings.ToLower(fedID) < strings.ToLower(existing.String) {
				if _, err := tx.ExecContext(ctx,
					`UPDATE "g_chat" SET "federation_id" = ? WHERE "id" = ?`, fedID, gChatID); err != nil {
					return 0, false, fmt.Errorf("settle direct chat id: %w", err)
				}
			}
			return gChatID, false, nil
		}
	}
	if !create {
		return 0, false, nil
	}
	if chatType != "group" {
		name = nil
	}
	gChatID, err = store.Insert(ctx, tx,
		`INSERT INTO "g_chat" ("type", "name", "created_by_email", "created_at", "federation_id") VALUES (?,?,?,?,?)`,
		chatType, name, creator, s.db.Now(), fedID)
	if err != nil {
		return 0, false, fmt.Errorf("create chat: %w", err)
	}
	return gChatID, true, nil
}

// checkSender refuses a message from someone who is not in a chat held here.
// Membership grows only by a member adding someone, never by a sender naming
// themselves (RFC 14.5). History is accepted from a sender who has left since,
// and a Domain's mailer-daemon is never a member.
func checkSender(ctx context.Context, tx *sql.Tx, gChatID int64, in federation.ChatInbound) error {
	if federation.IsMailerDaemon(in.SenderEmail) {
		return nil
	}
	query := `SELECT COUNT(*) FROM "g_chat_participant" WHERE "g_chat_id" = ? AND "email" = ? AND "left_at" IS NULL`
	if in.History {
		query = `SELECT COUNT(*) FROM "g_chat_participant" WHERE "g_chat_id" = ? AND "email" = ?`
	}
	n, err := store.SelectInt(ctx, tx, query, gChatID, in.SenderEmail)
	if err != nil {
		return fmt.Errorf("check chat membership: %w", err)
	}
	// Leaving is said by the leaver, in the message that says so.
	if n == 0 && !(contains(in.Left, in.SenderEmail) && wasMember(ctx, tx, gChatID, in.SenderEmail)) {
		return fmt.Errorf("%s in chat %s: %w", in.SenderEmail, in.ChatFedID, federation.ErrNotMember)
	}
	return nil
}

func wasMember(ctx context.Context, tx *sql.Tx, gChatID int64, address string) bool {
	n, err := store.SelectInt(ctx, tx,
		`SELECT COUNT(*) FROM "g_chat_participant" WHERE "g_chat_id" = ? AND "email" = ?`, gChatID, address)
	return err == nil && n > 0
}

// applyMembership records the members an envelope names that are not known
// here, and lets the sending Domain say which of its own people are no longer
// in the chat. No Domain removes people who live elsewhere, and an address of
// this Server's own with no account is not a member (RFC 5.3, 14.5). It
// returns who was added.
func (s *Service) applyMembership(ctx context.Context, tx *sql.Tx, gChatID int64, origin string,
	members, gone []string) ([]string, error) {
	known, err := store.SelectStrings(ctx, tx,
		`SELECT "email" FROM "g_chat_participant" WHERE "g_chat_id" = ?`, gChatID)
	if err != nil {
		return nil, fmt.Errorf("list chat participants: %w", err)
	}
	var added []string
	for _, address := range members {
		if contains(known, address) || contains(gone, address) {
			continue
		}
		if !s.remote(address) {
			n, err := store.SelectInt(ctx, tx, `SELECT COUNT(*) FROM "alias" WHERE "email" = ?`, address)
			if err != nil {
				return nil, fmt.Errorf("look up member: %w", err)
			}
			if n == 0 {
				continue
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "g_chat_participant" ("g_chat_id", "email", "role", "joined_at") VALUES (?,?,'member',?)`,
			gChatID, address, s.db.Now()); err != nil {
			return nil, fmt.Errorf("add participant %s: %w", address, err)
		}
		known = append(known, address)
		added = append(added, address)
	}
	for _, address := range gone {
		if !strings.EqualFold(federation.DomainOf(address), origin) {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE "g_chat_participant" SET "left_at" = ? WHERE "g_chat_id" = ? AND "email" = ? AND "left_at" IS NULL`,
			s.db.Now(), gChatID, address); err != nil {
			return nil, fmt.Errorf("record %s leaving: %w", address, err)
		}
	}
	return added, nil
}

// inboundMessage stores the canonical message once, whichever Domain sends it
// first (RFC 14.5).
func (s *Service) inboundMessage(ctx context.Context, tx *sql.Tx, gChatID int64, in federation.ChatInbound) (int64, error) {
	gMessageID, err := store.SelectInt(ctx, tx,
		`SELECT "id" FROM "g_chat_message" WHERE "federation_id" = ? LIMIT 1`, in.MessageFedID)
	if err != nil {
		return 0, fmt.Errorf("look up chat message: %w", err)
	}
	if gMessageID != 0 {
		return gMessageID, nil
	}
	name := in.SenderName
	if name == "" {
		name = in.SenderEmail
	}
	// The Origin's send time, not ours: it is what orders the chat (RFC 5.5).
	gMessageID, err = store.Insert(ctx, tx,
		`INSERT INTO "g_chat_message" ("g_chat_id", "sender_email", "sender_name", "body", "body_html", `+
			`"created_at", "federation_id") VALUES (?,?,?,?,?,?,?)`,
		gChatID, in.SenderEmail, name, in.Body, nullable(in.BodyHTML), in.SentAt, in.MessageFedID)
	if err != nil {
		return 0, fmt.Errorf("create chat message: %w", err)
	}
	for _, a := range in.Attachments {
		if err := s.storeAttachment(ctx, tx, gMessageID,
			NewAttachment{FileName: a.FileName, MimeType: a.MimeType, FileData: a.Data}); err != nil {
			return 0, err
		}
	}
	return gMessageID, nil
}

// projectInbound gives a recipient their copy of a message. A new message is
// unread and brings back a chat they deleted; history is neither (RFC 14.5,
// 14.6).
func (s *Service) projectInbound(ctx context.Context, tx *sql.Tx, gChatID, gMessageID int64, address string,
	history bool, now time.Time) error {
	var customerID, chatID int64
	err := tx.QueryRowContext(ctx,
		`SELECT "chat"."customer_id", "chat"."id" FROM "chat" `+
			`JOIN "alias" ON "alias"."customer_id" = "chat"."customer_id" `+
			`WHERE "alias"."email" = ? AND "chat"."g_chat_id" = ? LIMIT 1`, address, gChatID).Scan(&customerID, &chatID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up chat projection: %w", err)
	}
	has, err := store.SelectInt(ctx, tx,
		`SELECT COUNT(*) FROM "chat_message" WHERE "chat_id" = ? AND "g_chat_message_id" = ?`, chatID, gMessageID)
	if err != nil {
		return fmt.Errorf("look up chat message projection: %w", err)
	}
	if has > 0 {
		return nil
	}
	var readAt any
	if history {
		readAt = now
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "chat_message" ("customer_id", "chat_id", "g_chat_message_id", "delivered_at", "read_at") `+
			"VALUES (?,?,?,?,?)", customerID, chatID, gMessageID, now, readAt); err != nil {
		return fmt.Errorf("create chat message projection: %w", err)
	}
	if history {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE "chat" SET "deleted_at" = NULL WHERE "id" = ? AND "deleted_at" IS NOT NULL`, chatID); err != nil {
		return fmt.Errorf("restore deleted chat: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "message_status" ("g_chat_message_id", "recipient_email", "status", "timestamp") `+
			"VALUES (?,?,'delivered',?)", gMessageID, address, now); err != nil {
		return fmt.Errorf("record message status: %w", err)
	}
	return nil
}

// ReceiveRead records that a member on another Domain has read the messages
// of this Server's people up to one of them. A reader's position only moves
// forward, and a receipt about a message not held here changes nothing (RFC
// 14.7).
func (s *Service) ReceiveRead(ctx context.Context, in federation.ChatReadInbound) error {
	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		gChatID, _, err := s.inboundChat(ctx, tx, in.ChatFedID, in.Type, nil, in.Members, "", false)
		if err != nil || gChatID == 0 {
			return err
		}
		member, err := store.SelectInt(ctx, tx,
			`SELECT COUNT(*) FROM "g_chat_participant" WHERE "g_chat_id" = ? AND "email" = ? AND "left_at" IS NULL`,
			gChatID, in.ReaderEmail)
		if err != nil {
			return fmt.Errorf("check chat membership: %w", err)
		}
		if member == 0 {
			return fmt.Errorf("%s in chat %s: %w", in.ReaderEmail, in.ChatFedID, federation.ErrNotMember)
		}
		var upToAt time.Time
		err = tx.QueryRowContext(ctx,
			`SELECT "created_at" FROM "g_chat_message" WHERE "g_chat_id" = ? AND "federation_id" = ? LIMIT 1`,
			gChatID, in.UpTo).Scan(&upToAt)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look up read message: %w", err)
		}
		if len(in.Recipients) == 0 {
			return nil
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT "id", "created_at", "federation_id" FROM "g_chat_message" `+
				`WHERE "g_chat_id" = ? AND "federation_id" IS NOT NULL AND "sender_email" IN (`+
				store.Placeholders(len(in.Recipients))+`)`,
			append([]any{gChatID}, toArgs(in.Recipients)...)...)
		if err != nil {
			return fmt.Errorf("list read messages: %w", err)
		}
		var read []int64
		for rows.Next() {
			var id int64
			var at time.Time
			var fedID string
			if err := rows.Scan(&id, &at, &fedID); err != nil {
				rows.Close()
				return fmt.Errorf("scan read message: %w", err)
			}
			if !before(upToAt, in.UpTo, at, fedID) {
				read = append(read, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range read {
			n, err := store.Affected(ctx, tx,
				`UPDATE "message_status" SET "status" = 'read', "timestamp" = ? `+
					`WHERE "g_chat_message_id" = ? AND "recipient_email" = ? AND "status" <> 'read'`,
				in.ReadAt, id, in.ReaderEmail)
			if err != nil {
				return fmt.Errorf("record read: %w", err)
			}
			if n > 0 {
				continue
			}
			exists, err := store.SelectInt(ctx, tx,
				`SELECT COUNT(*) FROM "message_status" WHERE "g_chat_message_id" = ? AND "recipient_email" = ?`,
				id, in.ReaderEmail)
			if err != nil {
				return fmt.Errorf("look up message status: %w", err)
			}
			if exists == 0 {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO "message_status" ("g_chat_message_id", "recipient_email", "status", "timestamp") `+
						"VALUES (?,?,'read',?)", id, in.ReaderEmail, in.ReadAt); err != nil {
					return fmt.Errorf("record read: %w", err)
				}
			}
		}
		return nil
	})
}

// ChatDelivered records that a peer accepted a message for its recipients:
// the second tick on the sender's message (RFC 14.7).
func (s *Service) ChatDelivered(ctx context.Context, gMessageID int64, recipients []string) error {
	for _, address := range recipients {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE "message_status" SET "status" = 'delivered', "timestamp" = ? `+
				`WHERE "g_chat_message_id" = ? AND "recipient_email" = ? AND "status" = 'sent'`,
			s.db.Now(), gMessageID, strings.ToLower(address)); err != nil {
			return fmt.Errorf("record chat delivery: %w", err)
		}
	}
	return nil
}

// BounceChat tells the sender of a message, in the chat and nowhere else, who
// it did not reach and why. Recipients that do not exist, or whose Domain
// does not do chat, are no longer members, so the sender is told once (RFC
// 14.1, 6.5). The notice is local: it is not federated.
func (s *Service) BounceChat(ctx context.Context, b federation.ChatBounce) error {
	if b.GMessageID == 0 {
		return nil
	}
	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		var gChatID int64
		var sender string
		err := tx.QueryRowContext(ctx,
			`SELECT "g_chat_id", "sender_email" FROM "g_chat_message" WHERE "id" = ? LIMIT 1`, b.GMessageID).
			Scan(&gChatID, &sender)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look up bounced message: %w", err)
		}
		var customerID, chatID int64
		err = tx.QueryRowContext(ctx,
			`SELECT "chat"."customer_id", "chat"."id" FROM "chat" `+
				`JOIN "alias" ON "alias"."customer_id" = "chat"."customer_id" `+
				`WHERE "alias"."email" = ? AND "chat"."g_chat_id" = ? LIMIT 1`, sender, gChatID).
			Scan(&customerID, &chatID)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look up sender's chat: %w", err)
		}
		now := s.db.Now()
		if b.Refused {
			for _, address := range b.Recipients {
				if _, err := tx.ExecContext(ctx,
					`UPDATE "g_chat_participant" SET "left_at" = ? WHERE "g_chat_id" = ? AND "email" = ? `+
						`AND "left_at" IS NULL`, now, gChatID, strings.ToLower(address)); err != nil {
					return fmt.Errorf("drop refused member: %w", err)
				}
			}
		}
		gNoticeID, err := store.Insert(ctx, tx,
			`INSERT INTO "g_chat_message" ("g_chat_id", "sender_email", "sender_name", "body", "created_at") `+
				"VALUES (?,?,?,?,?)",
			gChatID, "mailer-daemon@"+federation.DomainOf(sender), "Mail Delivery System", bounceText(b), now)
		if err != nil {
			return fmt.Errorf("write chat bounce: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "chat_message" ("customer_id", "chat_id", "g_chat_message_id", "delivered_at") VALUES (?,?,?,?)`,
			customerID, chatID, gNoticeID, now); err != nil {
			return fmt.Errorf("deliver chat bounce: %w", err)
		}
		return nil
	})
}

func bounceText(b federation.ChatBounce) string {
	who := strings.Join(b.Recipients, ", ")
	switch {
	case b.Backfill:
		return fmt.Sprintf("Your message has not reached %s yet: %s did not answer. "+
			"It will be sent again when %s can be reached.", who, b.PeerDomain, b.PeerDomain)
	case b.Refused:
		return fmt.Sprintf("Your message could not be delivered to %s (%s answered %d %s). "+
			"They are not in this chat.", who, b.PeerDomain, b.HTTPStatus, b.ErrorCode)
	}
	return fmt.Sprintf("Your message could not be delivered to %s (%s answered %d %s).",
		who, b.PeerDomain, b.HTTPStatus, b.ErrorCode)
}

func toArgs(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
