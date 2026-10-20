package email

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mevdschee/webmail-server/internal/federation"
	"github.com/mevdschee/webmail-server/internal/mdconv"
	"github.com/mevdschee/webmail-server/internal/store"
)

// federate queues one envelope per destination Domain for the subscribers this
// Server cannot deliver to itself.
//
// One envelope per Domain, not per recipient: a Domain with three subscribers
// on a thread receives one envelope naming all three, and fans out locally. Each
// destination is a separate row, so a peer being down delays only its own
// envelope (RFC 6.4, design 4).
//
// Everything here runs inside the caller's transaction, which is the point: the
// canonical rows and the promise to deliver them commit together or not at all.
func (s *Service) federate(ctx context.Context, tx *sql.Tx, gThreadID, gMessageID int64,
	sentAt time.Time, sender identity, subscribers []string, remote map[string][]string,
	body, bodyHTML string, attachments []NewAttachment) error {
	if len(remote) == 0 {
		return nil
	}
	if s.federation == nil {
		return fmt.Errorf("remote recipients but no federation configured")
	}
	origin, ok := s.federation.OriginFor(sender.Email)
	if !ok {
		// The sender's own address is not on a Domain this Server hosts, so
		// there is no key to sign as and no origin to claim. Refusing is the
		// only honest answer: silently dropping would look like delivery.
		return fmt.Errorf("cannot federate from %s: it is not a Domain hosted here", sender.Email)
	}

	threadFedID, err := s.federationID(ctx, tx, "g_thread", gThreadID)
	if err != nil {
		return err
	}
	messageFedID, err := s.federationID(ctx, tx, "g_message", gMessageID)
	if err != nil {
		return err
	}
	subject, err := store.SelectString(ctx, tx,
		`SELECT "subject_line" FROM "g_thread" WHERE "id" = ? LIMIT 1`, gThreadID)
	if err != nil {
		return fmt.Errorf("look up subject: %w", err)
	}

	// in_reply_to is the thread's own federated id on every message after the
	// first. It is what lets the peer put a reply on the existing thread rather
	// than starting a new one, and a peer that has never seen it is allowed to
	// start a fresh thread instead (RFC 5.3).
	inReplyTo := ""
	priorMessages, err := store.SelectInt(ctx, tx,
		`SELECT COUNT(*) FROM "g_message" WHERE "g_thread_id" = ?`, gThreadID)
	if err != nil {
		return fmt.Errorf("count thread messages: %w", err)
	}
	if priorMessages > 1 {
		inReplyTo = threadFedID
	}

	outAttachments, err := s.federatedAttachments(ctx, tx, gMessageID, attachments)
	if err != nil {
		return err
	}

	// Plain text is what the protocol carries as the body; the HTML is optional
	// and goes alongside it, the same pair the local rows hold.
	if body == "" {
		body = mdconv.ToMarkdown(bodyHTML)
	}

	for domain, addresses := range remote {
		if err := s.federation.Enqueue(ctx, tx, federation.Outbound{
			PeerDomain:   domain,
			OriginDomain: origin.Domain,
			ThreadFedID:  threadFedID,
			InReplyTo:    inReplyTo,
			Subject:      subject,
			MessageFedID: messageFedID,
			SenderEmail:  sender.Email,
			SenderName:   sender.Name,
			Body:         body,
			BodyHTML:     bodyHTML,
			// The stored send time, not a second reading of the clock, which
			// could fall in the next second and order the copies differently.
			SentAt: sentAt,
			// Recipients are only this Domain's share (RFC 5.3). Members are
			// everyone, so a reply written there reaches every Domain.
			Recipients:  addresses,
			Members:     subscribers,
			Attachments: outAttachments,
			RelatedID:   gMessageID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// federationID reads a row's federated identifier, minting one if it has none.
//
// Threads created before federation was switched on have no identifier, and
// neither does any row this Server never needed to federate. Generating lazily
// means the identifier exists exactly when it is first needed and stays stable
// from then on, which is what reply correlation depends on.
func (s *Service) federationID(ctx context.Context, tx *sql.Tx, table string, id int64) (string, error) {
	// table is not user input: it is one of two literals chosen by the caller.
	var existing sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT "federation_id" FROM "`+table+`" WHERE "id" = ? LIMIT 1`, id).Scan(&existing)
	if err != nil {
		return "", fmt.Errorf("look up %s federation id: %w", table, err)
	}
	if existing.Valid && existing.String != "" {
		return existing.String, nil
	}
	fedID, err := federation.NewUUID()
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE "`+table+`" SET "federation_id" = ? WHERE "id" = ?`, fedID, id); err != nil {
		return "", fmt.Errorf("assign %s federation id: %w", table, err)
	}
	return fedID, nil
}

// federatedAttachments pairs the bytes just written with the federated ids of
// their canonical rows, so both sides of a transfer name the same attachment.
func (s *Service) federatedAttachments(ctx context.Context, tx *sql.Tx, gMessageID int64,
	attachments []NewAttachment) ([]federation.OutboundAttachment, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT "id", "file_name" FROM "g_attachment" WHERE "g_message_id" = ? ORDER BY "id"`, gMessageID)
	if err != nil {
		return nil, fmt.Errorf("list attachments to federate: %w", err)
	}
	defer rows.Close()
	var ids []int64
	var names []string
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		ids = append(ids, id)
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]federation.OutboundAttachment, 0, len(attachments))
	for i := range attachments {
		if i >= len(ids) {
			break
		}
		fedID, err := s.federationID(ctx, tx, "g_attachment", ids[i])
		if err != nil {
			return nil, err
		}
		name := attachments[i].FileName
		if i < len(names) && names[i] != "" {
			name = names[i]
		}
		out = append(out, federation.OutboundAttachment{
			FederationID: fedID,
			FileName:     name,
			MimeType:     attachments[i].MimeType,
			Data:         attachments[i].FileData,
		})
	}
	return out, nil
}
