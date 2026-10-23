package chat

import (
	"context"
	"testing"
	"time"

	"github.com/103mail/webmail-server/internal/blob"
	"github.com/103mail/webmail-server/internal/store"
	"github.com/103mail/webmail-server/internal/store/storetest"
)

// These tests run the chat domain's queries against a real database: SQLite
// by default, PostgreSQL when WEBMAIL_TEST_POSTGRES_DSN is set (ADR 19).

type fixture struct {
	t   *testing.T
	ctx context.Context
	db  *store.DB
	svc *Service
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := storetest.Open(t)
	f := &fixture{t: t, ctx: context.Background(), db: db,
		now: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
	db.Clock = func() time.Time { return f.now }
	f.svc = New(db, blob.New(t.TempDir()))
	return f
}

type person struct {
	customerID, userID, mailboxID int64
	email                         string
}

func (f *fixture) insert(query string, args ...any) int64 {
	f.t.Helper()
	id, err := store.Insert(f.ctx, f.db, query, args...)
	if err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return id
}

func (f *fixture) person(email, name string) person {
	f.t.Helper()
	p := person{email: email}
	p.customerID = f.insert(`INSERT INTO "customer" ("name") VALUES (?)`, name)
	p.userID = f.insert(`INSERT INTO "user" ("customer_id", "username", "password", "name") VALUES (?,?,?,?)`,
		p.customerID, email, "", name)
	p.mailboxID = f.insert(`INSERT INTO "mailbox" ("customer_id", "user_id", "name", "color") VALUES (?,?,?,?)`,
		p.customerID, p.userID, name, "red")
	alias := f.insert(`INSERT INTO "alias" ("customer_id", "email", "name", "mailbox_id") VALUES (?,?,?,?)`,
		p.customerID, email, name, p.mailboxID)
	if _, err := f.db.ExecContext(f.ctx, `UPDATE "mailbox" SET "default_alias_id" = ? WHERE "id" = ?`,
		alias, p.mailboxID); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) chatOf(p person, gChatID int64) int64 {
	f.t.Helper()
	id, err := store.SelectInt(f.ctx, f.db,
		`SELECT "id" FROM "chat" WHERE "customer_id" = ? AND "g_chat_id" = ?`, p.customerID, gChatID)
	if err != nil || id == 0 {
		f.t.Fatalf("no chat for %s: %v", p.email, err)
	}
	return id
}

func TestChatLifecycle(t *testing.T) {
	f := newFixture(t)
	alice := f.person("alice@example.test", "Alice")
	bob := f.person("bob@example.test", "Bob")
	carol := f.person("carol@example.test", "Carol")

	quiet, err := f.svc.AddChat(f.ctx, alice.customerID, alice.mailboxID, "direct", nil, []string{"CAROL@example.test"})
	if err != nil || quiet == 0 {
		t.Fatalf("AddChat = %d, %v", quiet, err)
	}
	name := "Planning"
	chatID, err := f.svc.AddChat(f.ctx, alice.customerID, alice.mailboxID, "group", &name, []string{"Bob@Example.test"})
	if err != nil || chatID == 0 {
		t.Fatalf("AddChat = %d, %v", chatID, err)
	}
	gChatID, err := f.svc.GChatID(f.ctx, alice.customerID, chatID)
	if err != nil || gChatID == 0 {
		t.Fatalf("GChatID = %d, %v", gChatID, err)
	}
	bobChat := f.chatOf(bob, gChatID)

	f.now = f.now.Add(time.Minute)
	messageID, err := f.svc.AddMessage(f.ctx, alice.customerID, chatID, "Kickoff agenda attached", "",
		[]NewAttachment{{FileName: "agenda.txt", MimeType: "text/plain", FileData: []byte("1. hello")}})
	if err != nil || messageID == 0 {
		t.Fatalf("AddMessage = %d, %v", messageID, err)
	}

	// The chat with a message sorts before the one without, on both engines.
	chats, err := f.svc.ListChats(f.ctx, alice.customerID, alice.mailboxID, "")
	if err != nil || len(chats) != 2 {
		t.Fatalf("ListChats = %+v, %v", chats, err)
	}
	if chats[0].ChatID != chatID || chats[0].LastMessageAt != "2026-10-20T09:01:00+00:00" || chats[1].LastMessageAt != "" {
		t.Errorf("chats in order %+v", chats)
	}
	if len(chats[0].Participants) != 2 {
		t.Errorf("participants %+v", chats[0].Participants)
	}
	bobChats, err := f.svc.ListChats(f.ctx, bob.customerID, bob.mailboxID, "")
	if err != nil || len(bobChats) != 1 || bobChats[0].UnreadCount != 1 {
		t.Fatalf("Bob's chats = %+v, %v", bobChats, err)
	}
	for search, want := range map[string]int{"agenda": 1, "kick*": 1, "alice": 1, "nothing": 0} {
		found, err := f.svc.ListChats(f.ctx, alice.customerID, alice.mailboxID, search)
		if err != nil {
			t.Fatalf("search %q: %v", search, err)
		}
		if len(found) != want {
			t.Errorf("search %q found %d chats, want %d", search, len(found), want)
		}
		messages, err := f.svc.ListMessages(f.ctx, bob.customerID, bobChat, search)
		if err != nil {
			t.Fatalf("search %q in messages: %v", search, err)
		}
		if len(messages) != want {
			t.Errorf("search %q found %d messages, want %d", search, len(messages), want)
		}
	}

	messages, err := f.svc.ListMessages(f.ctx, bob.customerID, bobChat, "")
	if err != nil || len(messages) != 1 {
		t.Fatalf("ListMessages = %+v, %v", messages, err)
	}
	m := messages[0]
	if m.DeliveryStatus == nil || m.DeliveryStatus.Sent != 2 || m.DeliveryStatus.Read != 1 || len(m.Attachments) != 1 {
		t.Errorf("message = %+v, status %+v", m, m.DeliveryStatus)
	}
	data, err := f.svc.GetAttachment(f.ctx, bob.customerID, m.Attachments[0].AttachmentID)
	if err != nil || data == nil || string(data.FileData) != "1. hello" {
		t.Fatalf("GetAttachment = %+v, %v", data, err)
	}
	if data, _ := f.svc.GetAttachment(f.ctx, carol.customerID, m.Attachments[0].AttachmentID); data != nil {
		t.Error("Carol read an attachment from a chat she is not in")
	}

	if err := f.svc.MarkChatRead(f.ctx, bob.customerID, bobChat); err != nil {
		t.Fatal(err)
	}
	if chats, _ := f.svc.ListChats(f.ctx, bob.customerID, bob.mailboxID, ""); chats[0].UnreadCount != 0 {
		t.Errorf("Bob has %d unread after reading", chats[0].UnreadCount)
	}

	// Only the author edits, and only inside the window.
	if ok, _ := f.svc.EditMessage(f.ctx, bob.customerID, messages[0].MessageID, "hijack", ""); ok {
		t.Error("Bob edited Alice's message")
	}
	if ok, err := f.svc.EditMessage(f.ctx, alice.customerID, messageID, "Kickoff agenda, revised", ""); err != nil || !ok {
		t.Fatalf("EditMessage = %v, %v", ok, err)
	}
	f.now = f.now.Add(EditWindow + time.Minute)
	if ok, _ := f.svc.DeleteMessage(f.ctx, alice.customerID, messageID); ok {
		t.Error("a message was deleted after the edit window")
	}

	muted := true
	pref := "mentions_only"
	renamed := "Planning Q4"
	if _, err := f.svc.EditChat(f.ctx, alice.customerID, chatID, &renamed, &muted, &pref); err != nil {
		t.Fatal(err)
	}
	if chats, _ := f.svc.ListChats(f.ctx, alice.customerID, alice.mailboxID, ""); !chats[0].Muted ||
		chats[0].NotificationPreference != "mentions_only" || *chats[0].Name != renamed {
		t.Errorf("edited chat = %+v", chats[0])
	}

	participantID, err := f.svc.AddParticipant(f.ctx, alice.customerID, chatID, "Carol@example.test", "member")
	if err != nil || participantID == 0 {
		t.Fatalf("AddParticipant = %d, %v", participantID, err)
	}
	if participants, err := f.svc.ListParticipants(f.ctx, alice.customerID, chatID); err != nil || len(participants) != 3 {
		t.Fatalf("ListParticipants = %+v, %v", participants, err)
	}
	if ok, err := f.svc.RemoveParticipant(f.ctx, alice.customerID, participantID); err != nil || !ok {
		t.Fatalf("RemoveParticipant = %v, %v", ok, err)
	}
	if ok, err := f.svc.DeleteChat(f.ctx, bob.customerID, bobChat); err != nil || !ok {
		t.Fatalf("DeleteChat = %v, %v", ok, err)
	}
	if chats, _ := f.svc.ListChats(f.ctx, bob.customerID, bob.mailboxID, ""); len(chats) != 0 {
		t.Error("Bob still sees the chat he deleted")
	}
}

func TestPresenceAndTyping(t *testing.T) {
	f := newFixture(t)
	alice := f.person("alice@example.test", "Alice")
	bob := f.person("bob@example.test", "Bob")
	chatID, err := f.svc.AddChat(f.ctx, alice.customerID, alice.mailboxID, "direct", nil, []string{bob.email})
	if err != nil {
		t.Fatal(err)
	}

	for _, status := range []string{"online", "away"} {
		if ok, err := f.svc.SetPresence(f.ctx, alice.customerID, alice.userID, status); err != nil || !ok {
			t.Fatalf("SetPresence(%s) = %v, %v", status, ok, err)
		}
	}
	presence, err := f.svc.GetPresence(f.ctx, alice.customerID, []int64{alice.userID, 999})
	if err != nil || len(presence) != 2 || presence[0].Status != "away" || presence[1].Status != "offline" {
		t.Fatalf("GetPresence = %+v, %v", presence, err)
	}
	f.now = f.now.Add(16 * time.Minute)
	if presence, _ := f.svc.GetPresence(f.ctx, alice.customerID, []int64{alice.userID}); presence[0].Status != "offline" {
		t.Errorf("stale presence reads %s", presence[0].Status)
	}

	if ok, err := f.svc.StartTyping(f.ctx, alice.customerID, alice.userID, chatID); err != nil || !ok {
		t.Fatalf("StartTyping = %v, %v", ok, err)
	}
	if ok, err := f.svc.StartTyping(f.ctx, alice.customerID, alice.userID, chatID); err != nil || !ok {
		t.Fatalf("StartTyping again = %v, %v", ok, err)
	}
	bobChat := f.chatOf(bob, mustGChat(t, f, alice, chatID))
	typing, err := f.svc.GetTyping(f.ctx, bob.customerID, bob.userID, bobChat)
	if err != nil || len(typing) != 1 || typing[0].UserName != "Alice" {
		t.Fatalf("GetTyping = %+v, %v", typing, err)
	}
	f.now = f.now.Add(TypingTTL)
	if typing, _ := f.svc.GetTyping(f.ctx, bob.customerID, bob.userID, bobChat); len(typing) != 0 {
		t.Errorf("an expired indicator is still shown: %+v", typing)
	}
	if ok, err := f.svc.StopTyping(f.ctx, alice.customerID, alice.userID, chatID); err != nil || !ok {
		t.Fatalf("StopTyping = %v, %v", ok, err)
	}
}

func mustGChat(t *testing.T, f *fixture, p person, chatID int64) int64 {
	t.Helper()
	id, err := f.svc.GChatID(f.ctx, p.customerID, chatID)
	if err != nil || id == 0 {
		t.Fatalf("GChatID = %d, %v", id, err)
	}
	return id
}

func TestSessions(t *testing.T) {
	f := newFixture(t)
	alice := f.person("alice@example.test", "Alice")
	bob := f.person("bob@example.test", "Bob")
	chatID, err := f.svc.AddChat(f.ctx, alice.customerID, alice.mailboxID, "direct", nil, []string{bob.email})
	if err != nil {
		t.Fatal(err)
	}
	gChatID := mustGChat(t, f, alice, chatID)

	aliceSession, err := f.svc.NewSession(f.ctx, alice.customerID, alice.userID)
	if err != nil {
		t.Fatal(err)
	}
	bobSession, err := f.svc.NewSession(f.ctx, bob.customerID, bob.userID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.svc.ClaimSession(f.ctx, aliceSession.ClientID)
	if err != nil || claimed.UserID != alice.userID {
		t.Fatalf("ClaimSession = %+v, %v", claimed, err)
	}
	if _, err := f.svc.ClaimSession(f.ctx, aliceSession.ClientID); !IsNoSession(err) {
		t.Errorf("a session was claimed twice: %v", err)
	}
	if _, err := f.svc.ClaimSession(f.ctx, "unknown"); !IsNoSession(err) {
		t.Errorf("an unknown session was claimed: %v", err)
	}

	// An unclaimed session expires.
	late, err := f.svc.NewSession(f.ctx, bob.customerID, bob.userID)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(SessionTTL + time.Second)
	if _, err := f.svc.ClaimSession(f.ctx, late.ClientID); !IsNoSession(err) {
		t.Errorf("an expired session was claimed: %v", err)
	}
	f.now = f.now.Add(-SessionTTL - time.Second)
	if _, err := f.svc.ClaimSession(f.ctx, bobSession.ClientID); err != nil {
		t.Fatal(err)
	}

	if live, err := f.svc.LiveSession(f.ctx, aliceSession.ClientID); err != nil || live.CustomerID != alice.customerID {
		t.Fatalf("LiveSession = %+v, %v", live, err)
	}
	targets, err := f.svc.SessionsForChat(f.ctx, gChatID)
	if err != nil || len(targets) != 2 {
		t.Fatalf("SessionsForChat = %+v, %v", targets, err)
	}
	peers, err := f.svc.SessionsForPeers(f.ctx, alice.customerID)
	if err != nil || len(peers) != 1 || peers[0] != bobSession.ClientID {
		t.Fatalf("SessionsForPeers = %v, %v", peers, err)
	}
	if email, err := f.svc.PrimaryAlias(f.ctx, alice.customerID); err != nil || email != alice.email {
		t.Errorf("PrimaryAlias = %q, %v", email, err)
	}

	if err := f.svc.CloseSession(f.ctx, aliceSession.ClientID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.LiveSession(f.ctx, aliceSession.ClientID); !IsNoSession(err) {
		t.Errorf("a closed session is live: %v", err)
	}

	// Bob goes quiet; the sweep closes his session, then deletes both.
	f.now = f.now.Add(10 * time.Minute)
	if err := f.svc.SweepSessions(f.ctx, 5*time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.LiveSession(f.ctx, bobSession.ClientID); !IsNoSession(err) {
		t.Errorf("an idle session survived the sweep: %v", err)
	}
	f.now = f.now.Add(2 * time.Hour)
	if err := f.svc.SweepSessions(f.ctx, 5*time.Minute, time.Hour); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.SelectInt(f.ctx, f.db, `SELECT COUNT(*) FROM "chat_session"`); n != 0 {
		t.Errorf("%d sessions left after the sweep", n)
	}
}
