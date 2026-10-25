package chat

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/store"
)

// Federator is what the chat domain needs of the federation service to reach
// members on other Domains (RFC 14). The types are the federation package's,
// which owns the wire, so the dependency runs one way.
type Federator interface {
	// IsLocal reports whether a DNS domain is one this Server hosts.
	IsLocal(domain string) bool
	// OriginFor returns the local Domain an address sends as.
	OriginFor(senderEmail string) (*federation.LocalDomain, bool)
	// EnqueueChat writes a chat envelope to the outbox in the caller's
	// transaction.
	EnqueueChat(ctx context.Context, tx *sql.Tx, out federation.ChatOutbound) error
}

// Federate lets chats reach members on other Domains. Without it every chat
// stays on this Server.
func (s *Service) Federate(f Federator) { s.fed = f }

// remote reports whether an address lives on another Domain.
func (s *Service) remote(address string) bool {
	return s.fed != nil && !s.fed.IsLocal(federation.DomainOf(address))
}

// remoteByDomain groups the addresses that live elsewhere by their Domain,
// which is who each envelope goes to (RFC 14.2).
func (s *Service) remoteByDomain(addresses []string) map[string][]string {
	out := map[string][]string{}
	for _, a := range addresses {
		if s.remote(a) && !federation.IsMailerDaemon(a) {
			domain := federation.DomainOf(a)
			out[domain] = append(out[domain], a)
		}
	}
	return out
}

// hasRemote reports whether any of the addresses lives elsewhere.
func (s *Service) hasRemote(addresses []string) bool {
	return len(s.remoteByDomain(addresses)) > 0
}

// chatFacts is what every envelope says about its chat.
type chatFacts struct {
	fedID   string
	kind    string
	name    *string
	members []string
}

// facts reads a chat's identity and its members, minting its federation id
// the first time it is needed. A direct chat's id is chosen by whoever writes
// first and settles on the lower of two when both did (RFC 14.3).
func (s *Service) facts(ctx context.Context, tx *sql.Tx, gChatID int64) (*chatFacts, error) {
	var f chatFacts
	var fedID, name sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT "type", "name", "federation_id" FROM "g_chat" WHERE "id" = ? LIMIT 1`, gChatID).
		Scan(&f.kind, &name, &fedID)
	if err != nil {
		return nil, fmt.Errorf("look up chat: %w", err)
	}
	if name.Valid && f.kind == "group" {
		f.name = &name.String
	}
	f.fedID = fedID.String
	if f.fedID == "" {
		if f.fedID, err = federation.NewUUID(); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE "g_chat" SET "federation_id" = ? WHERE "id" = ?`, f.fedID, gChatID); err != nil {
			return nil, fmt.Errorf("assign chat federation id: %w", err)
		}
	}
	if f.members, err = activeParticipants(ctx, tx, gChatID); err != nil {
		return nil, err
	}
	return &f, nil
}

// federateMessage queues a message just written here for every Domain that
// has a member of the chat, one envelope each, naming only that Domain's
// members (RFC 14.2, 14.4). The members elsewhere start as sent; a peer's 202
// makes them delivered and its receipts read (RFC 14.7). Members added by the
// message then get its earlier messages (RFC 14.5).
func (s *Service) federateMessage(ctx context.Context, tx *sql.Tx, gMessageID int64, fedID string,
	sentAt time.Time, m outgoing, participants []string) error {
	byDomain := s.remoteByDomain(participants)
	if len(byDomain) == 0 {
		return nil
	}
	origin, ok := s.fed.OriginFor(m.sender)
	if !ok {
		return fmt.Errorf("cannot federate from %s: it is not a Domain hosted here", m.sender)
	}
	f, err := s.facts(ctx, tx, m.gChatID)
	if err != nil {
		return err
	}
	attachments, err := outboundAttachments(m.attachments)
	if err != nil {
		return err
	}
	for _, domain := range sortedKeys(byDomain) {
		recipients := byDomain[domain]
		if err := s.fed.EnqueueChat(ctx, tx, federation.ChatOutbound{
			PeerDomain:   domain,
			OriginDomain: origin.Domain,
			Kind:         federation.KindChat,
			ChatFedID:    f.fedID,
			Type:         f.kind,
			Name:         f.name,
			Members:      f.members,
			Left:         m.left,
			Recipients:   recipients,
			MessageFedID: fedID,
			SenderEmail:  m.sender,
			SenderName:   m.senderName,
			Body:         m.body,
			BodyHTML:     m.bodyHTML,
			SentAt:       sentAt,
			Attachments:  attachments,
			RelatedID:    gMessageID,
		}); err != nil {
			return err
		}
		for _, address := range recipients {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO "message_status" ("g_chat_message_id", "recipient_email", "status", "timestamp") `+
					"VALUES (?,?,'sent',?)", gMessageID, address, sentAt); err != nil {
				return fmt.Errorf("record message status: %w", err)
			}
		}
	}
	return s.sendHistory(ctx, tx, m.gChatID, m.added, gMessageID)
}

// sendHistory sends members just added on other Domains the earlier messages
// this Server's own people wrote in the chat. Each Domain sends only its own
// users' messages, so every message stays signed by its sender's Domain, and
// the Domains that hold the chat between them send all of it (RFC 14.5).
func (s *Service) sendHistory(ctx context.Context, tx *sql.Tx, gChatID int64, added []string, except int64) error {
	byDomain := s.remoteByDomain(added)
	if len(byDomain) == 0 {
		return nil
	}
	f, err := s.facts(ctx, tx, gChatID)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT "id", "sender_email", "sender_name", "body", "body_html", "created_at", "federation_id" `+
			`FROM "g_chat_message" WHERE "g_chat_id" = ? AND "id" <> ? AND "deleted_at" IS NULL `+
			`ORDER BY "created_at", "id"`, gChatID, except)
	if err != nil {
		return fmt.Errorf("list chat history: %w", err)
	}
	type earlier struct {
		id                      int64
		sender, name, body, htm string
		at                      time.Time
		fedID                   string
	}
	var history []earlier
	for rows.Next() {
		var e earlier
		var html, fedID sql.NullString
		if err := rows.Scan(&e.id, &e.sender, &e.name, &e.body, &html, &e.at, &fedID); err != nil {
			rows.Close()
			return fmt.Errorf("scan chat history: %w", err)
		}
		e.htm, e.fedID = html.String, fedID.String
		if s.remote(e.sender) || federation.IsMailerDaemon(e.sender) {
			continue
		}
		history = append(history, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list chat history: %w", err)
	}

	for _, e := range history {
		origin, ok := s.fed.OriginFor(e.sender)
		if !ok {
			continue
		}
		// A message from before chat federated has no id yet; it gets one now
		// and keeps it.
		if e.fedID == "" {
			if e.fedID, err = federation.NewUUID(); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE "g_chat_message" SET "federation_id" = ? WHERE "id" = ?`, e.fedID, e.id); err != nil {
				return fmt.Errorf("assign chat message federation id: %w", err)
			}
		}
		stored, err := s.storedAttachments(ctx, tx, e.id)
		if err != nil {
			return err
		}
		attachments, err := outboundAttachments(stored)
		if err != nil {
			return err
		}
		for _, domain := range sortedKeys(byDomain) {
			if err := s.fed.EnqueueChat(ctx, tx, federation.ChatOutbound{
				PeerDomain:   domain,
				OriginDomain: origin.Domain,
				Kind:         federation.KindChat,
				ChatFedID:    f.fedID,
				Type:         f.kind,
				Name:         f.name,
				Members:      f.members,
				Recipients:   byDomain[domain],
				MessageFedID: e.fedID,
				SenderEmail:  e.sender,
				SenderName:   e.name,
				Body:         e.body,
				BodyHTML:     e.htm,
				SentAt:       e.at,
				History:      true,
				Attachments:  attachments,
				RelatedID:    e.id,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// storedAttachments reads a message's attachments back with their bytes.
func (s *Service) storedAttachments(ctx context.Context, tx *sql.Tx, gMessageID int64) ([]NewAttachment, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT "file_name", "mime_type", "hash_md5" FROM "g_chat_attachment" `+
			`WHERE "g_chat_message_id" = ? ORDER BY "id"`, gMessageID)
	if err != nil {
		return nil, fmt.Errorf("list chat attachments: %w", err)
	}
	type ref struct{ name, mime, hash string }
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.name, &r.mime, &r.hash); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan chat attachment: %w", err)
		}
		refs = append(refs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]NewAttachment, 0, len(refs))
	for _, r := range refs {
		data, err := s.blob.Get(r.hash)
		if err != nil {
			return nil, err
		}
		out = append(out, NewAttachment{FileName: r.name, MimeType: r.mime, FileData: data})
	}
	return out, nil
}

// outboundAttachments gives each attachment the identifier the envelope
// names it by. Chat attachments are not referred to again, so it is not kept.
func outboundAttachments(attachments []NewAttachment) ([]federation.OutboundAttachment, error) {
	out := make([]federation.OutboundAttachment, 0, len(attachments))
	for _, a := range attachments {
		id, err := federation.NewUUID()
		if err != nil {
			return nil, err
		}
		out = append(out, federation.OutboundAttachment{
			FederationID: id, FileName: a.FileName, MimeType: a.MimeType, Data: a.FileData,
		})
	}
	return out, nil
}

// unreadForReceipts is a message someone is about to read, written on
// another Domain.
type unreadForReceipts struct {
	sender string
	fedID  string
	at     time.Time
}

// newlyRead lists the messages in a customer's chat that are unread and were
// written elsewhere: the ones a receipt is owed for when they are read. A
// history message was projected read, so it is never among them (RFC 14.7).
func (s *Service) newlyRead(ctx context.Context, tx *sql.Tx, customerID, chatID int64) ([]unreadForReceipts, error) {
	if s.fed == nil {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT "g_chat_message"."sender_email", "g_chat_message"."federation_id", "g_chat_message"."created_at" `+
			`FROM "chat_message" JOIN "g_chat_message" ON "g_chat_message"."id" = "chat_message"."g_chat_message_id" `+
			`WHERE "chat_message"."customer_id" = ? AND "chat_message"."chat_id" = ? `+
			`AND "chat_message"."read_at" IS NULL AND "g_chat_message"."federation_id" IS NOT NULL`,
		customerID, chatID)
	if err != nil {
		return nil, fmt.Errorf("list unread chat messages: %w", err)
	}
	defer rows.Close()
	var out []unreadForReceipts
	for rows.Next() {
		var u unreadForReceipts
		if err := rows.Scan(&u.sender, &u.fedID, &u.at); err != nil {
			return nil, fmt.Errorf("scan unread chat message: %w", err)
		}
		if s.remote(u.sender) && !federation.IsMailerDaemon(u.sender) {
			out = append(out, u)
		}
	}
	return out, rows.Err()
}

// sendReceipts tells each Domain whose users wrote messages the reader has
// just read how far the reader got: the newest of those messages, and the
// users who wrote them. The rest of the chat learns nothing (RFC 14.7).
func (s *Service) sendReceipts(ctx context.Context, tx *sql.Tx, customerID, chatID int64, read []unreadForReceipts) error {
	if len(read) == 0 {
		return nil
	}
	var gChatID, mailboxID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT "g_chat_id", "mailbox_id" FROM "chat" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
		customerID, chatID).Scan(&gChatID, &mailboxID); err != nil {
		return fmt.Errorf("look up chat: %w", err)
	}
	reader, err := store.SelectString(ctx, tx,
		`SELECT "email" FROM "alias" WHERE "customer_id" = ? AND "mailbox_id" = ? LIMIT 1`, customerID, mailboxID)
	if err != nil {
		return fmt.Errorf("look up reader: %w", err)
	}
	origin, ok := s.fed.OriginFor(reader)
	if !ok {
		return nil
	}
	f, err := s.facts(ctx, tx, gChatID)
	if err != nil {
		return err
	}
	if !contains(f.members, reader) {
		return nil
	}
	byDomain := map[string][]unreadForReceipts{}
	for _, u := range read {
		domain := federation.DomainOf(u.sender)
		byDomain[domain] = append(byDomain[domain], u)
	}
	now := s.db.Now()
	for _, domain := range sortedKeys(byDomain) {
		messages := byDomain[domain]
		sort.Slice(messages, func(i, j int) bool {
			return before(messages[i].at, messages[i].fedID, messages[j].at, messages[j].fedID)
		})
		var writers []string
		for _, m := range messages {
			if !contains(writers, m.sender) {
				writers = append(writers, m.sender)
			}
		}
		if err := s.fed.EnqueueChat(ctx, tx, federation.ChatOutbound{
			PeerDomain:   domain,
			OriginDomain: origin.Domain,
			Kind:         federation.KindChatRead,
			ChatFedID:    f.fedID,
			Type:         f.kind,
			Name:         f.name,
			Members:      f.members,
			Recipients:   writers,
			ReaderEmail:  reader,
			UpTo:         messages[len(messages)-1].fedID,
			ReadAt:       now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// before is the order of a chat's messages on every Domain: by send time in
// whole seconds, then by federation id (RFC 5.5, 14.4).
func before(at time.Time, fedID string, otherAt time.Time, otherFedID string) bool {
	a, b := at.Truncate(time.Second), otherAt.Truncate(time.Second)
	if !a.Equal(b) {
		return a.Before(b)
	}
	return strings.ToLower(fedID) < strings.ToLower(otherFedID)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
