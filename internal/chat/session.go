package chat

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/103mail/webmail-server/internal/store"
)

// SessionTTL is how long a minted ClientId may go unclaimed. It only has to
// cover the round trip from minting one to opening the socket, so it is short:
// an id that leaks from a log is worth nothing a minute later.
const SessionTTL = 2 * time.Minute

// Session is one WebSocket connection's identity.
type Session struct {
	ClientID   string `json:"clientId"`
	CustomerID int64  `json:"-"`
	UserID     int64  `json:"-"`
	ExpiresAt  string `json:"expiresAt"`
}

// NewSession mints a ClientId for an authenticated user (ADR 18).
//
// The id is opaque and carries nothing: not a user id, not a mailbox id, not a
// token. It travels in URL paths and proxy logs, so anything it encoded would
// be disclosed by both.
func (s *Service) NewSession(ctx context.Context, customerID, userID int64) (*Session, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate client id: %w", err)
	}
	clientID := base64.RawURLEncoding.EncodeToString(buf)
	expires := s.db.Now().Add(SessionTTL)

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO "chat_session" ("client_id", "customer_id", "user_id", "expires_at") `+
			"VALUES (?,?,?,?)", clientID, customerID, userID, expires); err != nil {
		return nil, fmt.Errorf("create chat session: %w", err)
	}
	return &Session{
		ClientID:   clientID,
		CustomerID: customerID,
		UserID:     userID,
		ExpiresAt:  expires.UTC().Format(time.RFC3339),
	}, nil
}

// ClaimSession resolves a ClientId at connect time and marks it connected.
//
// It is the authentication step for the whole socket: the proxy refuses the
// upgrade unless this succeeds. A session may be claimed once. A second attempt
// on the same id fails, so a ClientId observed in a log cannot be used to open a
// parallel connection as that user.
func (s *Service) ClaimSession(ctx context.Context, clientID string) (*Session, error) {
	// One conditional update is the claim: it succeeds for exactly one caller
	// on either engine, without a row lock SQLite could not take.
	var session Session
	now := s.db.Now()
	err := s.db.QueryRowContext(ctx,
		`UPDATE "chat_session" SET "connected_at" = ?, "last_seen_at" = ? `+
			`WHERE "client_id" = ? AND "connected_at" IS NULL AND "closed_at" IS NULL `+
			`AND "expires_at" >= ? RETURNING "customer_id", "user_id"`,
		now, now, clientID, now).Scan(&session.CustomerID, &session.UserID)
	if err == sql.ErrNoRows {
		return nil, errNoSession
	}
	if err != nil {
		return nil, fmt.Errorf("claim chat session: %w", err)
	}
	session.ClientID = clientID
	return &session, nil
}

// errNoSession covers every reason a ClientId is not usable: unknown, expired,
// already claimed, already closed. They are deliberately indistinguishable to
// the caller, which answers a flat refusal either way.
var errNoSession = fmt.Errorf("no such chat session")

// IsNoSession reports whether an error means the ClientId was not usable.
func IsNoSession(err error) bool { return err == errNoSession }

// LiveSession returns the identity behind a connected ClientId and records that
// it is still alive. Every frame goes through it, so it doubles as the heartbeat
// that keeps presence fresh.
func (s *Service) LiveSession(ctx context.Context, clientID string) (*Session, error) {
	var session Session
	err := s.db.QueryRowContext(ctx,
		`SELECT "customer_id", "user_id" FROM "chat_session" `+
			`WHERE "client_id" = ? AND "connected_at" IS NOT NULL AND "closed_at" IS NULL LIMIT 1`,
		clientID).Scan(&session.CustomerID, &session.UserID)
	if err == sql.ErrNoRows {
		return nil, errNoSession
	}
	if err != nil {
		return nil, fmt.Errorf("look up live chat session: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE "chat_session" SET "last_seen_at" = ? WHERE "client_id" = ?`, s.db.Now(), clientID); err != nil {
		return nil, fmt.Errorf("touch chat session: %w", err)
	}
	session.ClientID = clientID
	return &session, nil
}

// CloseSession marks a session gone. Called when the proxy reports a
// disconnect, and when a push finds the client is no longer there.
func (s *Service) CloseSession(ctx context.Context, clientID string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE "chat_session" SET "closed_at" = ? WHERE "client_id" = ? AND "closed_at" IS NULL`,
		s.db.Now(), clientID); err != nil {
		return fmt.Errorf("close chat session: %w", err)
	}
	return nil
}

// Target is one open connection to push to, and the chat id that connection
// knows the conversation by.
//
// The chat id is per-customer: a chat is a canonical g_chat with one projection
// row per participant, so the same conversation is a different id to each of
// them. A frame addressed with the sender's id would be about a chat the
// recipient has never heard of.
type Target struct {
	ClientID string
	ChatID   int64
	// CustomerID is whose connection it is, which a frame built for that
	// participant alone needs.
	CustomerID int64
}

// SessionsForChat returns every open connection of a chat's participants, each
// with that participant's own id for the chat. This is the fan-out list: one
// HTTP post each, because a stateless API tier has nothing to broadcast over
// (ADR 13).
func (s *Service) SessionsForChat(ctx context.Context, gChatID int64) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT "s"."client_id", "c"."id", "c"."customer_id" FROM "chat_session" "s" `+
			`JOIN "chat" "c" ON "c"."customer_id" = "s"."customer_id" AND "c"."g_chat_id" = ? `+
			`WHERE "c"."deleted_at" IS NULL `+
			`AND "s"."connected_at" IS NOT NULL AND "s"."closed_at" IS NULL`, gChatID)
	if err != nil {
		return nil, fmt.Errorf("list sessions for chat: %w", err)
	}
	defer rows.Close()

	var targets []Target
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.ClientID, &t.ChatID, &t.CustomerID); err != nil {
			return nil, fmt.Errorf("scan target: %w", err)
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

// SessionsForPeers returns the open ClientIds of everyone who shares a chat with
// a user, which is who a presence change concerns. Someone with no chat in
// common has no business knowing whether this user is online.
func (s *Service) SessionsForPeers(ctx context.Context, customerID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT "s"."client_id" FROM "chat_session" "s" `+
			`JOIN "alias" "a" ON "a"."customer_id" = "s"."customer_id" `+
			`JOIN "g_chat_participant" "p" ON "p"."email" = "a"."email" AND "p"."left_at" IS NULL `+
			`WHERE "p"."g_chat_id" IN (`+
			`  SELECT "mine"."g_chat_id" FROM "g_chat_participant" "mine" `+
			`  JOIN "alias" "ma" ON "ma"."email" = "mine"."email" `+
			`  WHERE "ma"."customer_id" = ? AND "mine"."left_at" IS NULL) `+
			`AND "s"."connected_at" IS NOT NULL AND "s"."closed_at" IS NULL `+
			`AND "s"."customer_id" <> ?`, customerID, customerID)
	if err != nil {
		return nil, fmt.Errorf("list peer sessions: %w", err)
	}
	defer rows.Close()
	return collectStrings(rows)
}

// PrimaryAlias returns the address a customer sends chat as. Presence is
// broadcast with it alongside the user id, because a chat lists its
// participants by address and the client has no way to join the two otherwise.
func (s *Service) PrimaryAlias(ctx context.Context, customerID int64) (string, error) {
	email, err := store.SelectString(ctx, s.db,
		`SELECT "email" FROM "alias" WHERE "customer_id" = ? ORDER BY "id" LIMIT 1`, customerID)
	if err != nil {
		return "", fmt.Errorf("look up primary alias: %w", err)
	}
	return email, nil
}

// GChatID resolves a customer's chat projection to the canonical chat, which is
// what the participant tables are keyed by.
func (s *Service) GChatID(ctx context.Context, customerID, chatID int64) (int64, error) {
	id, err := store.SelectInt(ctx, s.db,
		`SELECT "g_chat_id" FROM "chat" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
		customerID, chatID)
	if err != nil {
		return 0, fmt.Errorf("look up canonical chat: %w", err)
	}
	return id, nil
}

// SweepSessions closes sessions that were never claimed and ones that have gone
// quiet, and deletes the rows that have been closed long enough to be of no
// further interest.
//
// The quiet-session rule is what covers a proxy that was killed: no disconnect
// ever arrives, so nothing else would ever mark those rows closed.
func (s *Service) SweepSessions(ctx context.Context, idleAfter, keepClosed time.Duration) error {
	now := s.db.Now()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE "chat_session" SET "closed_at" = ? `+
			`WHERE "closed_at" IS NULL AND "connected_at" IS NULL AND "expires_at" < ?`, now, now); err != nil {
		return fmt.Errorf("close unclaimed chat sessions: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE "chat_session" SET "closed_at" = ? `+
			`WHERE "closed_at" IS NULL AND "connected_at" IS NOT NULL AND "last_seen_at" < ?`,
		now, now.Add(-idleAfter)); err != nil {
		return fmt.Errorf("close idle chat sessions: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM "chat_session" WHERE "closed_at" < ?`, now.Add(-keepClosed)); err != nil {
		return fmt.Errorf("delete closed chat sessions: %w", err)
	}
	return nil
}

func collectStrings(rows *sql.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// MarkChatRead advances a chat's read marker to its newest message and marks
// every delivered message in it read.
//
// Reading is what clears an unread count, and until now only sending did that.
// The socket makes opening a conversation observable, so this is where it lands.
func (s *Service) MarkChatRead(ctx context.Context, customerID, chatID int64) error {
	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		gChatID, err := store.SelectInt(ctx, tx,
			`SELECT "g_chat_id" FROM "chat" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
			customerID, chatID)
		if err != nil {
			return fmt.Errorf("look up chat: %w", err)
		}
		if gChatID == 0 {
			return nil
		}
		newest, err := store.SelectInt(ctx, tx,
			`SELECT MAX("id") FROM "g_chat_message" WHERE "g_chat_id" = ? AND "deleted_at" IS NULL`,
			gChatID)
		if err != nil {
			return fmt.Errorf("look up newest message: %w", err)
		}
		if newest == 0 {
			return nil
		}
		// What is about to be read for the first time, and was written
		// elsewhere, is what the writers' Domains get a receipt for (RFC 14.7).
		read, err := s.newlyRead(ctx, tx, customerID, chatID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE "chat_message" SET "read_at" = ? `+
				`WHERE "customer_id" = ? AND "chat_id" = ? AND "read_at" IS NULL`,
			s.db.Now(), customerID, chatID); err != nil {
			return fmt.Errorf("mark messages read: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE "chat" SET "last_read_message_id" = ? WHERE "customer_id" = ? AND "id" = ?`,
			newest, customerID, chatID); err != nil {
			return fmt.Errorf("advance read marker: %w", err)
		}
		return s.sendReceipts(ctx, tx, customerID, chatID, read)
	})
}
