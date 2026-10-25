package chat

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/store"
)

// fakeFederator hosts example.test and keeps what the chat domain enqueues.
type fakeFederator struct{ sent []federation.ChatOutbound }

func (f *fakeFederator) IsLocal(domain string) bool { return domain == "example.test" }
func (f *fakeFederator) OriginFor(email string) (*federation.LocalDomain, bool) {
	if federation.DomainOf(email) != "example.test" {
		return nil, false
	}
	return &federation.LocalDomain{Domain: "example.test"}, true
}
func (f *fakeFederator) EnqueueChat(ctx context.Context, tx *sql.Tx, out federation.ChatOutbound) error {
	f.sent = append(f.sent, out)
	return nil
}

// take returns what went out since it was last called.
func (f *fakeFederator) take() []federation.ChatOutbound {
	sent := f.sent
	f.sent = nil
	return sent
}

func newFederatedFixture(t *testing.T) (*fixture, *fakeFederator) {
	f := newFixture(t)
	fed := &fakeFederator{}
	f.svc.Federate(fed)
	return f, fed
}

func (f *fixture) gChatOf(p person, chatID int64) int64 {
	f.t.Helper()
	id, err := f.svc.GChatID(f.ctx, p.customerID, chatID)
	if err != nil || id == 0 {
		f.t.Fatalf("no canonical chat for %d: %v", chatID, err)
	}
	return id
}

func (f *fixture) bodies(p person, chatID int64) []string {
	f.t.Helper()
	messages, err := f.svc.ListMessages(f.ctx, p.customerID, chatID, "")
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for i := len(messages) - 1; i >= 0; i-- {
		out = append(out, messages[i].SenderEmail+": "+messages[i].Body)
	}
	return out
}

// A message in a chat with members elsewhere goes to each of their Domains
// once, naming only that Domain's members, and carries everyone as members.
// Their ticks start at sent, and move with the peer's 202 and its receipts
// (RFC 14.2, 14.7).
func TestMessagesReachEveryOtherDomainOnce(t *testing.T) {
	f, fed := newFederatedFixture(t)
	alice := f.person("alice@example.test", "Alice")
	ann := f.person("ann@example.test", "Ann")
	name := "Launch"
	chatID, err := f.svc.AddChat(f.ctx, alice.customerID, alice.mailboxID, "group", &name,
		[]string{ann.email, "bob@peer.test", "carol@peer.test", "erin@acme.test"})
	if err != nil {
		t.Fatal(err)
	}
	created := fed.take()
	if len(created) != 2 {
		t.Fatalf("creating the group sent %d envelopes, want one per other Domain", len(created))
	}
	for _, out := range created {
		if out.Body != "Alice created the group Launch" || out.Type != "group" || *out.Name != "Launch" ||
			len(out.Members) != 5 || out.ChatFedID == "" || out.MessageFedID == "" {
			t.Errorf("creation envelope %+v", out)
		}
	}
	byPeer := map[string][]string{}
	for _, out := range created {
		byPeer[out.PeerDomain] = out.Recipients
	}
	if !slices.Equal(byPeer["peer.test"], []string{"bob@peer.test", "carol@peer.test"}) ||
		!slices.Equal(byPeer["acme.test"], []string{"erin@acme.test"}) {
		t.Errorf("recipients by Domain %v", byPeer)
	}

	if _, err := f.svc.AddMessage(f.ctx, alice.customerID, chatID, "Can everyone review?", "", nil); err != nil {
		t.Fatal(err)
	}
	sent := fed.take()
	if len(sent) != 2 || sent[0].ChatFedID != created[0].ChatFedID {
		t.Fatalf("a message sent %+v", sent)
	}
	status := func() DeliveryStatus {
		messages, err := f.svc.ListMessages(f.ctx, alice.customerID, chatID, "")
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(messages, func(m Message) bool { return m.Body == "Can everyone review?" })
		return *messages[i].DeliveryStatus
	}
	if got := status(); got != (DeliveryStatus{Sent: 5, Delivered: 2, Read: 1}) {
		t.Errorf("before any peer answered: %+v", got)
	}
	gMessageID := sent[0].RelatedID
	if err := f.svc.ChatDelivered(f.ctx, gMessageID, []string{"bob@peer.test", "carol@peer.test"}); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != (DeliveryStatus{Sent: 5, Delivered: 4, Read: 1}) {
		t.Errorf("after peer.test took it: %+v", got)
	}
	if err := f.svc.ReceiveRead(f.ctx, federation.ChatReadInbound{
		ChatFedID: sent[0].ChatFedID, Type: "group", ReaderEmail: "bob@peer.test",
		UpTo: sent[0].MessageFedID, ReadAt: f.now, Recipients: []string{alice.email},
	}); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != (DeliveryStatus{Sent: 5, Delivered: 4, Read: 2}) {
		t.Errorf("after bob read it: %+v", got)
	}
	// A receipt from someone not in the chat is refused for now.
	err = f.svc.ReceiveRead(f.ctx, federation.ChatReadInbound{
		ChatFedID: sent[0].ChatFedID, Type: "group", ReaderEmail: "mallory@peer.test",
		UpTo: sent[0].MessageFedID, ReadAt: f.now, Recipients: []string{alice.email},
	})
	if !errors.Is(err, federation.ErrNotMember) {
		t.Errorf("a non-member's receipt: %v", err)
	}
}

func inbound(fedID, chatType string, name *string, members []string, sender, body string, at time.Time,
	recipients ...string) federation.ChatInbound {
	return federation.ChatInbound{
		OriginDomain: federation.DomainOf(sender), ChatFedID: fedID, Type: chatType, Name: name,
		Members: members, MessageFedID: newID(), SenderEmail: sender, SenderName: strings.Split(sender, "@")[0],
		Body: body, SentAt: at, Recipients: recipients,
	}
}

func newID() string {
	id, err := federation.NewUUID()
	if err != nil {
		panic(err)
	}
	return id
}

// A chat from another Domain is created for its recipients, in the order its
// messages were written rather than the order they arrived, and nobody who
// does not exist here, nor anyone who names themselves a member, gets in
// (RFC 14.3, 14.4, 14.5).
func TestInboundChatsAreCreatedAndKept(t *testing.T) {
	f, _ := newFederatedFixture(t)
	bob := f.person("bob@example.test", "Bob")
	name := "Launch"
	chat := newID()
	members := []string{"alice@peer.test", "bob@example.test", "ghost@example.test"}
	first := inbound(chat, "group", &name, members, "alice@peer.test", "Alice created the group Launch",
		f.now, "bob@example.test", "ghost@example.test")
	var pushed []int64
	f.svc.OnReceived(func(ctx context.Context, gChatID, gMessageID int64) { pushed = append(pushed, gMessageID) })
	n, refused, err := f.svc.DeliverChat(f.ctx, first)
	if err != nil || n != 1 || !slices.Equal(refused, []string{"ghost@example.test"}) {
		t.Fatalf("deliver = %d, %v, %v", n, refused, err)
	}
	if len(pushed) != 1 {
		t.Fatalf("%d messages pushed, want the one received", len(pushed))
	}
	bobChat := mustChat(t, f, bob)
	if m, err := f.svc.MessageByCanonical(f.ctx, bob.customerID, bobChat, pushed[0]); err != nil || m == nil ||
		m.Body != "Alice created the group Launch" {
		t.Errorf("the pushed message reads %+v, %v", m, err)
	}
	if again, _, err := f.svc.DeliverChat(f.ctx, first); err != nil || again != 1 {
		t.Fatalf("a repeat = %d, %v", again, err)
	}

	later := inbound(chat, "group", &name, members[:2], "alice@peer.test", "second", f.now.Add(2*time.Second), "bob@example.test")
	earlier := inbound(chat, "group", &name, members[:2], "alice@peer.test", "first", f.now.Add(time.Second), "bob@example.test")
	for _, in := range []federation.ChatInbound{later, earlier} {
		if _, _, err := f.svc.DeliverChat(f.ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	chats, err := f.svc.ListChats(f.ctx, bob.customerID, bob.mailboxID, "")
	if err != nil || len(chats) != 1 {
		t.Fatalf("bob has %d chats, %v", len(chats), err)
	}
	if chats[0].UnreadCount != 3 || *chats[0].Name != "Launch" || len(chats[0].Participants) != 2 {
		t.Errorf("bob's chat %+v", chats[0])
	}
	want := []string{"alice@peer.test: Alice created the group Launch", "alice@peer.test: first", "alice@peer.test: second"}
	if got := f.bodies(bob, chats[0].ChatID); !slices.Equal(got, want) {
		t.Errorf("bob reads %q, want %q", got, want)
	}

	intruder := inbound(chat, "group", &name, append(members[:2], "mallory@peer.test"), "mallory@peer.test", "hi",
		f.now, "bob@example.test")
	if _, _, err := f.svc.DeliverChat(f.ctx, intruder); !errors.Is(err, federation.ErrNotMember) {
		t.Errorf("someone who named themselves a member: %v", err)
	}
}

// Two people who start a direct chat with each other at the same moment end
// up in one chat, under the lower of the two ids (RFC 14.3).
func TestDirectChatsSettleOnOne(t *testing.T) {
	f, fed := newFederatedFixture(t)
	bob := f.person("bob@example.test", "Bob")
	chatID, err := f.svc.AddChat(f.ctx, bob.customerID, bob.mailboxID, "direct", nil, []string{"alice@peer.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AddMessage(f.ctx, bob.customerID, chatID, "Lunch?", "", nil); err != nil {
		t.Fatal(err)
	}
	mine := fed.take()[0].ChatFedID
	theirs := "00000000-0000-7000-8000-000000000001"
	in := inbound(theirs, "direct", nil, []string{"alice@peer.test", "bob@example.test"}, "alice@peer.test",
		"Lunch tomorrow?", f.now, "bob@example.test")
	if _, _, err := f.svc.DeliverChat(f.ctx, in); err != nil {
		t.Fatal(err)
	}
	chats, _ := f.svc.ListChats(f.ctx, bob.customerID, bob.mailboxID, "")
	if len(chats) != 1 {
		t.Fatalf("bob has %d direct chats with alice, want one", len(chats))
	}
	if _, err := f.svc.AddMessage(f.ctx, bob.customerID, chatID, "Sure", "", nil); err != nil {
		t.Fatal(err)
	}
	if got := fed.take()[0].ChatFedID; got != min(mine, theirs) {
		t.Errorf("bob's chat is now %s, want the lower of %s and %s", got, mine, theirs)
	}
}

// History reaches a newcomer read and without bringing back a chat they
// deleted; a new message does both. A member who left gets nothing new, and
// the Domain that sent their notice is the only one that can say so (RFC
// 14.5, 14.6).
func TestHistoryLeavingAndDeleting(t *testing.T) {
	f, _ := newFederatedFixture(t)
	bob := f.person("bob@example.test", "Bob")
	carol := f.person("carol@example.test", "Carol")
	name := "Launch"
	chat := newID()
	members := []string{"alice@peer.test", "bob@example.test"}
	if _, _, err := f.svc.DeliverChat(f.ctx, inbound(chat, "group", &name, members, "alice@peer.test",
		"hello", f.now, "bob@example.test")); err != nil {
		t.Fatal(err)
	}
	bobChat := f.chatOf(bob, f.gChatOf(bob, mustChat(t, f, bob)))
	if _, err := f.svc.DeleteChat(f.ctx, bob.customerID, bobChat); err != nil {
		t.Fatal(err)
	}
	old := inbound(chat, "group", &name, members, "alice@peer.test", "from before", f.now.Add(-time.Hour), "bob@example.test")
	old.History = true
	if _, _, err := f.svc.DeliverChat(f.ctx, old); err != nil {
		t.Fatal(err)
	}
	if chats, _ := f.svc.ListChats(f.ctx, bob.customerID, bob.mailboxID, ""); len(chats) != 0 {
		t.Error("history brought back a deleted chat")
	}
	fresh := inbound(chat, "group", &name, members, "alice@peer.test", "still there?", f.now, "bob@example.test")
	if _, _, err := f.svc.DeliverChat(f.ctx, fresh); err != nil {
		t.Fatal(err)
	}
	chats, _ := f.svc.ListChats(f.ctx, bob.customerID, bob.mailboxID, "")
	if len(chats) != 1 || chats[0].UnreadCount != 2 {
		t.Fatalf("after a new message bob has %+v, want the chat back with 2 unread", chats)
	}

	// Alice adds Carol: Carol sees what this Domain holds, read.
	withCarol := append(slices.Clone(members), carol.email)
	add := inbound(chat, "group", &name, withCarol, "alice@peer.test", "Alice added carol", f.now, bob.email, carol.email)
	if _, _, err := f.svc.DeliverChat(f.ctx, add); err != nil {
		t.Fatal(err)
	}
	carolChats, _ := f.svc.ListChats(f.ctx, carol.customerID, carol.mailboxID, "")
	if len(carolChats) != 1 || carolChats[0].UnreadCount != 1 {
		t.Fatalf("carol has %+v, want the chat with only the addition unread", carolChats)
	}
	if got := f.bodies(carol, carolChats[0].ChatID); len(got) != 4 {
		t.Errorf("carol reads %q, want the whole chat", got)
	}

	// Another Domain cannot say Bob left; Alice's can say Alice did.
	notBob := inbound(chat, "group", &name, []string{"alice@peer.test", carol.email}, "alice@peer.test", "bye bob",
		f.now, carol.email)
	notBob.Left = []string{bob.email}
	if _, _, err := f.svc.DeliverChat(f.ctx, notBob); err != nil {
		t.Fatal(err)
	}
	if chats, _ := f.svc.ListChats(f.ctx, bob.customerID, bob.mailboxID, ""); len(chats[0].Participants) != 3 {
		t.Errorf("peer.test removed bob: %+v", chats[0].Participants)
	}
	leave := inbound(chat, "group", &name, []string{bob.email, carol.email}, "alice@peer.test", "Alice left", f.now,
		bob.email, carol.email)
	leave.Left = []string{"alice@peer.test"}
	if _, _, err := f.svc.DeliverChat(f.ctx, leave); err != nil {
		t.Fatal(err)
	}
	after := inbound(chat, "group", &name, withCarol, "alice@peer.test", "I'm back", f.now, bob.email)
	if _, _, err := f.svc.DeliverChat(f.ctx, after); !errors.Is(err, federation.ErrNotMember) {
		t.Errorf("a member who left wrote again: %v", err)
	}
}

func mustChat(t *testing.T, f *fixture, p person) int64 {
	t.Helper()
	chats, err := f.svc.ListChats(f.ctx, p.customerID, p.mailboxID, "")
	if err != nil || len(chats) != 1 {
		t.Fatalf("%s has %d chats, %v", p.email, len(chats), err)
	}
	return chats[0].ChatID
}

// Adding someone elsewhere tells every Domain with a message, and sends the
// newcomer's Domain this Server's own earlier messages as history. Leaving is
// said by the leaver's Domain, and nobody here can remove someone who lives
// elsewhere (RFC 14.5).
func TestJoiningAndLeavingAreFederated(t *testing.T) {
	f, fed := newFederatedFixture(t)
	alice := f.person("alice@example.test", "Alice")
	name := "Launch"
	chatID, err := f.svc.AddChat(f.ctx, alice.customerID, alice.mailboxID, "group", &name, []string{"bob@peer.test"})
	if err != nil {
		t.Fatal(err)
	}
	fed.take()
	if _, err := f.svc.AddParticipant(f.ctx, alice.customerID, chatID, "erin@acme.test", "member"); err != nil {
		t.Fatal(err)
	}
	var notices, history []federation.ChatOutbound
	for _, out := range fed.take() {
		if out.History {
			history = append(history, out)
		} else {
			notices = append(notices, out)
		}
	}
	if len(notices) != 2 || notices[0].Body != "Alice added erin@acme.test" || !slices.Contains(notices[0].Members, "erin@acme.test") {
		t.Errorf("the addition went out as %+v", notices)
	}
	if len(history) != 1 || history[0].PeerDomain != "acme.test" || history[0].Body != "Alice created the group Launch" ||
		!slices.Equal(history[0].Recipients, []string{"erin@acme.test"}) {
		t.Errorf("history went out as %+v", history)
	}

	participants, err := f.svc.ListParticipants(f.ctx, alice.customerID, chatID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range participants {
		if p.Email == "bob@peer.test" {
			if ok, err := f.svc.RemoveParticipant(f.ctx, alice.customerID, p.ParticipantID); ok || err != nil {
				t.Errorf("alice removed bob, who lives elsewhere: %v, %v", ok, err)
			}
		}
	}
	for _, p := range participants {
		if p.Email == alice.email {
			if ok, err := f.svc.RemoveParticipant(f.ctx, alice.customerID, p.ParticipantID); !ok || err != nil {
				t.Fatalf("alice could not leave: %v, %v", ok, err)
			}
		}
	}
	left := fed.take()
	if len(left) != 2 || left[0].Body != "Alice left the chat" || !slices.Equal(left[0].Left, []string{alice.email}) ||
		slices.Contains(left[0].Members, alice.email) {
		t.Errorf("leaving went out as %+v", left)
	}
}

// Reading a chat sends each Domain whose people wrote what was read a receipt
// up to the newest of their messages, and nobody else one (RFC 14.7).
func TestReadingSendsReceiptsToTheWriters(t *testing.T) {
	f, fed := newFederatedFixture(t)
	bob := f.person("bob@example.test", "Bob")
	ann := f.person("ann@example.test", "Ann")
	name := "Launch"
	chat := newID()
	members := []string{"alice@peer.test", "erin@acme.test", bob.email, ann.email}
	var newestFromPeer string
	for i, sender := range []string{"alice@peer.test", "alice@peer.test", "erin@acme.test"} {
		in := inbound(chat, "group", &name, members, sender, "m", f.now.Add(time.Duration(i)*time.Second), bob.email, ann.email)
		if sender == "alice@peer.test" {
			newestFromPeer = in.MessageFedID
		}
		if _, _, err := f.svc.DeliverChat(f.ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	fed.take()
	bobChat := mustChat(t, f, bob)
	if _, err := f.svc.AddMessage(f.ctx, ann.customerID, mustChat(t, f, ann), "local", "", nil); err != nil {
		t.Fatal(err)
	}
	fed.take()
	if err := f.svc.MarkChatRead(f.ctx, bob.customerID, bobChat); err != nil {
		t.Fatal(err)
	}
	receipts := fed.take()
	if len(receipts) != 2 {
		t.Fatalf("%d receipts, want one for each Domain with a writer: %+v", len(receipts), receipts)
	}
	for _, r := range receipts {
		if r.Kind != federation.KindChatRead || r.ReaderEmail != bob.email {
			t.Errorf("receipt %+v", r)
		}
		if r.PeerDomain == "peer.test" && (r.UpTo != newestFromPeer || !slices.Equal(r.Recipients, []string{"alice@peer.test"})) {
			t.Errorf("peer.test receipt up to %s for %v, want %s for alice", r.UpTo, r.Recipients, newestFromPeer)
		}
	}
	if err := f.svc.MarkChatRead(f.ctx, bob.customerID, bobChat); err != nil {
		t.Fatal(err)
	}
	if again := fed.take(); len(again) != 0 {
		t.Errorf("reading again sent %d receipts", len(again))
	}
}

// A message a peer refused tells its sender in the chat, and only them, and
// those it did not reach are no longer members. Messages in a chat with
// members elsewhere cannot be edited (RFC 14.1, 14.6).
func TestBouncesAndEditsInFederatedChats(t *testing.T) {
	f, fed := newFederatedFixture(t)
	alice := f.person("alice@example.test", "Alice")
	ann := f.person("ann@example.test", "Ann")
	name := "Launch"
	chatID, err := f.svc.AddChat(f.ctx, alice.customerID, alice.mailboxID, "group", &name,
		[]string{ann.email, "bob@peer.test"})
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := f.svc.AddMessage(f.ctx, alice.customerID, chatID, "hello", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.svc.EditMessage(f.ctx, alice.customerID, messageID, "changed", ""); ok || err != nil {
		t.Errorf("a federated message was edited: %v, %v", ok, err)
	}
	gMessageID := fed.take()[1].RelatedID
	if err := f.svc.BounceChat(f.ctx, federation.ChatBounce{
		GMessageID: gMessageID, Recipients: []string{"bob@peer.test"}, PeerDomain: "peer.test",
		HTTPStatus: 400, ErrorCode: "unsupported_version", Refused: true,
	}); err != nil {
		t.Fatal(err)
	}
	got := f.bodies(alice, chatID)
	if last := got[len(got)-1]; !strings.HasPrefix(last, "mailer-daemon@example.test: ") || !strings.Contains(last, "bob@peer.test") {
		t.Errorf("alice's chat ends with %q, want a notice naming bob", last)
	}
	if annGot := f.bodies(ann, f.chatOf(ann, f.gChatOf(alice, chatID))); slices.ContainsFunc(annGot, func(b string) bool {
		return strings.HasPrefix(b, "mailer-daemon")
	}) {
		t.Error("the notice reached someone other than the sender")
	}
	participants, _ := f.svc.ListParticipants(f.ctx, alice.customerID, chatID)
	if slices.ContainsFunc(participants, func(p Participant) bool { return p.Email == "bob@peer.test" }) {
		t.Error("a refused member is still in the chat")
	}
	if n, _ := store.SelectInt(f.ctx, f.db, `SELECT COUNT(*) FROM "g_chat_message" WHERE "federation_id" IS NULL`); n != 1 {
		t.Errorf("%d messages without an id, want only the local notice", n)
	}
}
