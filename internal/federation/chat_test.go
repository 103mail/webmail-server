package federation

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/103mail/webmail-server/internal/store"
	"github.com/103mail/webmail-server/internal/store/storetest"
)

func validChatEnvelope() ChatEnvelope {
	name := "Launch"
	attachments := []Attachment{}
	return ChatEnvelope{
		Version:      ChatVersion,
		Kind:         KindChat,
		EnvelopeID:   "018f4d2b-9f21-7890-abcd-ef01234567a0",
		OriginDomain: "103mail.com",
		OriginSentAt: FormatTimestamp(time.Now()),
		Chat: Chat{
			FederationID: "018f4d2b-9f21-7890-abcd-ef01234567a1",
			Type:         "group",
			Name:         &name,
			Members:      []string{"alice@103mail.com", "bob@example.com", "erin@mail.acme.io"},
		},
		Message: &ChatMessage{Message: Message{
			FederationID: "018f4d2b-9f21-7890-abcd-ef01234567a2",
			SenderEmail:  "alice@103mail.com",
			SenderName:   "Alice",
			Body:         "Docs are ready",
			SentAt:       FormatTimestamp(time.Now()),
		}},
		Attachments: &attachments,
		Recipients:  []string{"bob@example.com"},
	}
}

func validReceipt() ChatEnvelope {
	e := validChatEnvelope()
	e.Kind = KindChatRead
	e.OriginDomain = "example.com"
	e.Message, e.Attachments = nil, nil
	e.Read = &ChatRead{
		ReaderEmail: "bob@example.com",
		UpTo:        "018f4d2b-9f21-7890-abcd-ef01234567a2",
		ReadAt:      FormatTimestamp(time.Now()),
	}
	e.Recipients = []string{"alice@103mail.com"}
	return e
}

// Every field rule of RFC 14.2 and 14.7 rejects, so the chat domain only
// ever sees a chat it can make sense of.
func TestChatEnvelopeFieldRules(t *testing.T) {
	if e := validChatEnvelope(); e.Validate(DefaultMaxAttachmentBytes) != nil {
		t.Fatalf("a valid chat envelope was rejected: %v", e.Validate(DefaultMaxAttachmentBytes))
	}
	if e := validReceipt(); e.Validate(DefaultMaxAttachmentBytes) != nil {
		t.Fatalf("a valid receipt was rejected: %v", e.Validate(DefaultMaxAttachmentBytes))
	}
	direct := func(e *ChatEnvelope) {
		e.Chat.Type, e.Chat.Name = "direct", nil
		e.Chat.Members = []string{"alice@103mail.com", "bob@example.com"}
	}
	for _, accepted := range []struct {
		name   string
		mutate func(*ChatEnvelope)
	}{
		{"a direct chat", direct},
		{"the sender leaving with the message", func(e *ChatEnvelope) {
			e.Chat.Members = []string{"bob@example.com", "erin@mail.acme.io"}
			e.Chat.Left = []string{"alice@103mail.com"}
		}},
		{"history from someone who has left", func(e *ChatEnvelope) {
			e.Chat.Members = []string{"bob@example.com", "erin@mail.acme.io"}
			e.Message.History = true
		}},
		{"no attachments field", func(e *ChatEnvelope) { e.Attachments = nil }},
	} {
		e := validChatEnvelope()
		accepted.mutate(&e)
		if r := e.Validate(DefaultMaxAttachmentBytes); r != nil {
			t.Errorf("%s was rejected: %v", accepted.name, r)
		}
	}

	name := "x"
	for _, tt := range []struct {
		name   string
		base   func() ChatEnvelope
		mutate func(*ChatEnvelope)
		code   string
	}{
		{"version 0.2", validChatEnvelope, func(e *ChatEnvelope) { e.Version = Version }, ErrUnsupportedVersion},
		{"an unknown kind", validChatEnvelope, func(e *ChatEnvelope) { e.Kind = "chat_typing" }, ErrSchemaViolation},
		{"a thread", validChatEnvelope, func(e *ChatEnvelope) { e.Thread = json.RawMessage(`{"subject":"x"}`) }, ErrSchemaViolation},
		{"a direct chat with a name", validChatEnvelope, func(e *ChatEnvelope) { direct(e); e.Chat.Name = &name }, ErrSchemaViolation},
		{"a direct chat of three", validChatEnvelope, func(e *ChatEnvelope) {
			direct(e)
			e.Chat.Members = append(e.Chat.Members, "erin@mail.acme.io")
		}, ErrSchemaViolation},
		{"a group without a name", validChatEnvelope, func(e *ChatEnvelope) { e.Chat.Name = nil }, ErrSchemaViolation},
		{"an unknown chat type", validChatEnvelope, func(e *ChatEnvelope) { e.Chat.Type = "channel" }, ErrSchemaViolation},
		{"a sender who is not a member", validChatEnvelope, func(e *ChatEnvelope) {
			e.Chat.Members = []string{"bob@example.com"}
		}, ErrSchemaViolation},
		{"a recipient who is not a member", validChatEnvelope, func(e *ChatEnvelope) {
			e.Recipients = []string{"carol@example.com"}
		}, ErrSchemaViolation},
		{"someone who left and is a member", validChatEnvelope, func(e *ChatEnvelope) {
			e.Chat.Left = []string{"bob@example.com"}
		}, ErrSchemaViolation},
		{"a chat message without one", validChatEnvelope, func(e *ChatEnvelope) { e.Message = nil }, ErrSchemaViolation},
		{"a chat message with a read", validChatEnvelope, func(e *ChatEnvelope) { e.Read = validReceipt().Read }, ErrSchemaViolation},
		{"a receipt with a message", validReceipt, func(e *ChatEnvelope) { e.Message = validChatEnvelope().Message }, ErrSchemaViolation},
		{"a receipt with attachments", validReceipt, func(e *ChatEnvelope) { e.Attachments = &[]Attachment{} }, ErrSchemaViolation},
		{"a receipt for a reader elsewhere", validReceipt, func(e *ChatEnvelope) { e.Read.ReaderEmail = "alice@103mail.com" }, ErrSchemaViolation},
		{"a receipt up to no message", validReceipt, func(e *ChatEnvelope) { e.Read.UpTo = "latest" }, ErrSchemaViolation},
	} {
		e := tt.base()
		tt.mutate(&e)
		r := e.Validate(DefaultMaxAttachmentBytes)
		if r == nil || r.Code != tt.code {
			t.Errorf("%s: got %v, want %s", tt.name, r, tt.code)
		}
	}
}

// recordingChats keeps what the outbox and the inbound endpoint ask of the
// chat domain.
type recordingChats struct {
	delivered  []ChatInbound
	reads      []ChatReadInbound
	accepted   map[int64][]string
	bounces    []ChatBounce
	refuse     []string
	notMember  bool
	undelivers int
}

func (c *recordingChats) DeliverChat(ctx context.Context, in ChatInbound) (int, []string, error) {
	if c.notMember {
		return 0, nil, ErrNotMember
	}
	c.delivered = append(c.delivered, in)
	return len(in.Recipients) - len(c.refuse), c.refuse, nil
}
func (c *recordingChats) UndeliveredChat(ctx context.Context, in ChatInbound) ([]string, error) {
	c.undelivers++
	return c.refuse, nil
}
func (c *recordingChats) ReceiveRead(ctx context.Context, in ChatReadInbound) error {
	if c.notMember {
		return ErrNotMember
	}
	c.reads = append(c.reads, in)
	return nil
}
func (c *recordingChats) ChatDelivered(ctx context.Context, gMessageID int64, recipients []string) error {
	if c.accepted == nil {
		c.accepted = map[int64][]string{}
	}
	c.accepted[gMessageID] = append(c.accepted[gMessageID], recipients...)
	return nil
}
func (c *recordingChats) BounceChat(ctx context.Context, b ChatBounce) error {
	c.bounces = append(c.bounces, b)
	return nil
}

// A Domain advertises chat, and version 0.3, only when it can receive one
// (RFC 14.1).
func TestChatIsAdvertisedWithTheChatDomain(t *testing.T) {
	s := newStoreService(t, storetest.Open(t))
	d, _ := s.DescriptorFor("example.test")
	if d.Chat || !slices.Equal(d.Versions, []string{Version}) {
		t.Errorf("without chat: chat %v, versions %v", d.Chat, d.Versions)
	}
	s.AttachChat(&recordingChats{})
	d, _ = s.DescriptorFor("example.test")
	if !d.Chat || !slices.Equal(d.Versions, []string{Version, ChatVersion}) {
		t.Errorf("with chat: chat %v, versions %v", d.Chat, d.Versions)
	}
	if !s.speaks(ChatVersion) || s.speaks("0.9") {
		t.Error("speaks does not follow Versions")
	}
}

func enqueueChat(t *testing.T, s *Service, out ChatOutbound) {
	t.Helper()
	ctx := context.Background()
	if err := s.db.Tx(ctx, func(tx *sql.Tx) error { return s.EnqueueChat(ctx, tx, out) }); err != nil {
		t.Fatal(err)
	}
}

func chatMessageOut(peer string) ChatOutbound {
	name := "Launch"
	return ChatOutbound{
		PeerDomain: peer, OriginDomain: "example.test", Kind: KindChat,
		ChatFedID: "0192f0c0-0000-7000-8000-0000000000c1", Type: "group", Name: &name,
		Members:      []string{"alice@example.test", "bob@" + peer, "carol@" + peer},
		Recipients:   []string{"bob@" + peer, "carol@" + peer},
		MessageFedID: "0192f0c0-0000-7000-8000-0000000000c2", SenderEmail: "alice@example.test",
		SenderName: "Alice", Body: "Docs are ready", SentAt: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
		RelatedID: 42,
	}
}

// What EnqueueChat writes is a chat envelope that a Destination accepts, and
// a receipt carries no message.
func TestEnqueuedChatEnvelopesAreValid(t *testing.T) {
	db := storetest.Open(t)
	s := newStoreService(t, db)
	enqueueChat(t, s, chatMessageOut("peer.test"))
	receipt := chatMessageOut("peer.test")
	receipt.Kind, receipt.ReaderEmail, receipt.UpTo = KindChatRead, "alice@example.test", receipt.MessageFedID
	receipt.ReadAt, receipt.Recipients = receipt.SentAt, []string{"bob@peer.test"}
	enqueueChat(t, s, receipt)

	rows, err := db.QueryContext(context.Background(),
		`SELECT "kind", "body" FROM "federation_outbox" ORDER BY "id"`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind, body string
		if err := rows.Scan(&kind, &body); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, kind)
		var e ChatEnvelope
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			t.Fatal(err)
		}
		if r := e.Validate(DefaultMaxAttachmentBytes); r != nil {
			t.Errorf("%s envelope is invalid: %v\n%s", kind, r, body)
		}
		if envelopeVersion([]byte(body)) != ChatVersion {
			t.Errorf("%s envelope is not version %s", kind, ChatVersion)
		}
	}
	if !slices.Equal(kinds, []string{"chat", "chat_read"}) {
		t.Errorf("outbox kinds %v", kinds)
	}
}

// Chat has a loop of its own: neither loop claims the other's rows, so mail
// queued for a slow peer cannot hold a chat up (RFC 14.4).
func TestChatAndMailDrainApart(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	s := newStoreService(t, db)
	enqueue(t, s, "mail.test", 2)
	enqueueChat(t, s, chatMessageOut("chat.test"))

	mailPeers, _ := s.duePeers(ctx, false)
	chatPeers, _ := s.duePeers(ctx, true)
	if !slices.Equal(mailPeers, []string{"mail.test"}) || !slices.Equal(chatPeers, []string{"chat.test"}) {
		t.Errorf("mail loop sees %v, chat loop sees %v", mailPeers, chatPeers)
	}
	if rows, _ := s.claim(ctx, "chat.test", 10, false); len(rows) != 0 {
		t.Errorf("the mail loop claimed %d chat rows", len(rows))
	}
	rows, err := s.claim(ctx, "chat.test", 10, true)
	if err != nil || len(rows) != 1 || rows[0].Kind != "chat" || rows[0].RelatedID != 42 {
		t.Fatalf("chat claim = %+v, %v", rows, err)
	}
}

// A chat envelope retries from a second up to a minute, for an hour, and is
// then owed like mail; its sender hears about it in the chat (RFC 14.4).
func TestChatRetriesAreShortAndThenOwed(t *testing.T) {
	db := storetest.Open(t)
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	db.Clock = func() time.Time { return now }
	ctx := context.Background()
	s := newStoreService(t, db)
	chats := &recordingChats{}
	s.AttachChat(chats)
	if s.cfg.ChatRetryBudget != ChatRetryBudget {
		t.Fatalf("chat budget %s, want %s", s.cfg.ChatRetryBudget, ChatRetryBudget)
	}
	enqueueChat(t, s, chatMessageOut("peer.test"))
	rows, _ := s.claim(ctx, "peer.test", 10, true)
	rows[0].Attempts = 10
	s.reschedule(ctx, rows[0], 503, "unavailable", 0)
	var next time.Time
	if err := db.QueryRowContext(ctx, `SELECT "next_retry_at" FROM "federation_outbox"`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if wait := next.Sub(now); wait < ChatRetryBase || wait > ChatRetryCap+ChatRetryBase {
		t.Errorf("a chat retry waits %s, want between %s and %s", wait, ChatRetryBase, ChatRetryCap+ChatRetryBase)
	}

	now = now.Add(ChatRetryBudget + time.Second)
	rows, _ = s.claim(ctx, "peer.test", 10, true)
	if len(rows) != 1 {
		t.Fatalf("claimed %d rows", len(rows))
	}
	s.attempt(ctx, rows[0])
	if n, _ := store.SelectInt(ctx, db, `SELECT COUNT(*) FROM "federation_outbox" WHERE "status" = 'owed'`); n != 1 {
		t.Errorf("%d owed rows, want the timed-out chat envelope", n)
	}
	if len(chats.bounces) != 1 || !chats.bounces[0].Backfill || chats.bounces[0].Refused ||
		chats.bounces[0].GMessageID != 42 || len(chats.bounces[0].Recipients) != 2 {
		t.Errorf("bounces = %+v, want one that promises a resend to both", chats.bounces)
	}

	// Backfill sends it again as a chat envelope, for the chat loop.
	if err := s.requeueOwed(ctx, "peer.test"); err != nil {
		t.Fatal(err)
	}
	if peers, _ := s.duePeers(ctx, true); !slices.Equal(peers, []string{"peer.test"}) {
		t.Errorf("the backfilled envelope is not for the chat loop: %v", peers)
	}
}

// A 202 marks the message delivered to the recipients the peer took, and
// bounces the ones it refused, who are then not members (RFC 14.7, 6.5). A
// permanent refusal of the whole envelope bounces everyone in it.
func TestChatOutcomesReachTheChatDomain(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	s := newStoreService(t, db)
	chats := &recordingChats{}
	s.AttachChat(chats)
	enqueueChat(t, s, chatMessageOut("peer.test"))
	enqueueChat(t, s, chatMessageOut("peer.test"))
	rows, _ := s.claim(ctx, "peer.test", 10, true)

	s.afterChatAttempt(ctx, rows[0], 202, "", []Refusal{
		{Address: "carol@peer.test", Error: ErrUnknownRecipient},
		{Address: "mallory@peer.test", Error: ErrUnknownRecipient},
	}, true, false, false)
	if got := chats.accepted[42]; !slices.Equal(got, []string{"bob@peer.test"}) {
		t.Errorf("delivered to %v, want only bob", got)
	}
	if len(chats.bounces) != 1 || !chats.bounces[0].Refused ||
		!slices.Equal(chats.bounces[0].Recipients, []string{"carol@peer.test"}) {
		t.Errorf("bounces = %+v, want carol refused and nobody it was not sent to", chats.bounces)
	}

	s.fail(ctx, rows[1], 400, ErrUnsupportedVersion)
	if len(chats.bounces) != 2 || !chats.bounces[1].Refused || len(chats.bounces[1].Recipients) != 2 {
		t.Errorf("bounce for a peer without chat = %+v", chats.bounces[1:])
	}
}

func chatRequest(t *testing.T, e ChatEnvelope) *Request {
	t.Helper()
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return &Request{Sender: e.OriginDomain, Destination: "example.test", Version: ChatVersion, Body: body}
}

func chatTo(e ChatEnvelope, origin string) ChatEnvelope {
	e.OriginDomain = origin
	e.Message.SenderEmail = "alice@" + origin
	e.Chat.Members = []string{"alice@" + origin, "bob@example.test"}
	e.Recipients = []string{"bob@example.test"}
	return e
}

// An authenticated chat envelope reaches the chat domain, once: a replay is
// answered from what was delivered, a sender who is not a member is told to
// come back later, and a Domain speaks only for its own people (RFC 14).
func TestInboundChatEnvelopes(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	s := newStoreService(t, db)

	if _, r := s.receiveChat(ctx, chatRequest(t, chatTo(validChatEnvelope(), "peer.test"))); r == nil ||
		r.Code != ErrUnsupportedKind {
		t.Errorf("a Server without chat accepted one: %v", r)
	}
	chats := &recordingChats{}
	s.AttachChat(chats)

	e := chatTo(validChatEnvelope(), "peer.test")
	if a, r := s.receiveChat(ctx, chatRequest(t, e)); r != nil || a.EnvelopeID != e.EnvelopeID {
		t.Fatalf("receive = %+v, %v", a, r)
	}
	if len(chats.delivered) != 1 || chats.delivered[0].MessageFedID != e.Message.FederationID ||
		chats.delivered[0].ChatFedID != e.Chat.FederationID {
		t.Fatalf("delivered %+v", chats.delivered)
	}
	if _, r := s.receiveChat(ctx, chatRequest(t, e)); r != nil || len(chats.delivered) != 1 || chats.undelivers != 1 {
		t.Errorf("a replay was delivered again: %v, %d deliveries", r, len(chats.delivered))
	}

	forged := chatTo(validChatEnvelope(), "peer.test")
	forged.EnvelopeID = "018f4d2b-9f21-7890-abcd-ef01234567b9"
	forged.Message.SenderEmail = "alice@103mail.com"
	forged.Chat.Members = append(forged.Chat.Members, "alice@103mail.com")
	if _, r := s.receiveChat(ctx, chatRequest(t, forged)); r == nil || r.Code != ErrSenderMismatch {
		t.Errorf("a peer spoke for another Domain's user: %v", r)
	}

	receipt := validReceipt()
	receipt.OriginDomain = "peer.test"
	receipt.EnvelopeID = "018f4d2b-9f21-7890-abcd-ef01234567b1"
	receipt.Read.ReaderEmail = "pete@peer.test"
	receipt.Chat.Members = []string{"pete@peer.test", "bob@example.test"}
	receipt.Recipients = []string{"bob@example.test"}
	if _, r := s.receiveChat(ctx, chatRequest(t, receipt)); r != nil || len(chats.reads) != 1 ||
		chats.reads[0].ReaderEmail != "pete@peer.test" {
		t.Errorf("receipt = %v, reads %+v", r, chats.reads)
	}

	chats.notMember = true
	late := chatTo(validChatEnvelope(), "peer.test")
	late.EnvelopeID = "018f4d2b-9f21-7890-abcd-ef01234567c0"
	_, r := s.receiveChat(ctx, chatRequest(t, late))
	if r == nil || r.Status != http.StatusTooEarly || r.Code != ErrNotAMember {
		t.Errorf("a non-member answered %v, want 425 not_a_member", r)
	}
	if seen, _ := s.alreadyAccepted(ctx, "peer.test", late.EnvelopeID); seen {
		t.Error("a refused envelope was recorded as accepted, so its retry would be a replay")
	}
}
