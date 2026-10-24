package email

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/103mail/webmail-server/internal/blob"
	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/store"
	"github.com/103mail/webmail-server/internal/store/storetest"
)

// These tests run the mail domain's queries against a real database: SQLite
// by default, PostgreSQL when WEBMAIL_TEST_POSTGRES_DSN is set (ADR 19). They
// check the projection rules end to end, so a query that only one engine
// accepts, or that the engines answer differently, fails here.

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
	f.svc = New(db, blob.New(t.TempDir()), nil, nil, nil)
	f.exec(`INSERT INTO "domain" ("name") VALUES (?)`, "example.test")
	return f
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.ExecContext(f.ctx, query, args...); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
}

func (f *fixture) count(query string, args ...any) int64 {
	f.t.Helper()
	n, err := store.SelectInt(f.ctx, f.db, query, args...)
	if err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// account creates a customer with a mailbox sending as email, and returns the
// customer, the mailbox and its inbox.
func (f *fixture) account(email, name string) (customerID, mailboxID, inboxID int64) {
	f.t.Helper()
	customerID, err := store.Insert(f.ctx, f.db, `INSERT INTO "customer" ("name") VALUES (?)`, name)
	if err != nil {
		f.t.Fatal(err)
	}
	mailboxID, err = f.svc.AddMailbox(f.ctx, customerID, name, "red", email, name)
	if err != nil || mailboxID == 0 {
		f.t.Fatalf("AddMailbox(%s) = %d, %v", email, mailboxID, err)
	}
	// AddMailbox sends as a generated address; make the named one the default.
	if _, err := f.svc.EditMailbox(f.ctx, customerID, 0, mailboxID, "", "", nil, nil, email, name); err != nil {
		f.t.Fatal(err)
	}
	inboxID, err = f.svc.DefaultFolderID(f.ctx, customerID, mailboxID)
	if err != nil || inboxID == 0 {
		f.t.Fatalf("DefaultFolderID = %d, %v", inboxID, err)
	}
	return customerID, mailboxID, inboxID
}

func TestThreadLifecycle(t *testing.T) {
	f := newFixture(t)
	alice, aliceMailbox, aliceInbox := f.account("alice@example.test", "Alice")
	bob, _, bobInbox := f.account("bob@example.test", "Bob")

	// Mixed case is stored lowercase, so it reaches Bob.
	threadID, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"Bob@Example.TEST"},
		"Quarterly report", "<p>The <b>numbers</b> are in.</p>",
		[]NewAttachment{{FileName: "q3.csv", MimeType: "text/csv", FileData: []byte("a,b\n1,2\n")}})
	if err != nil || threadID == 0 {
		t.Fatalf("AddThread = %d, %v", threadID, err)
	}

	bobThreads, err := f.svc.ListThreads(f.ctx, bob, bobInbox, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(bobThreads) != 1 || bobThreads[0].Subject != "Quarterly report" ||
		bobThreads[0].UnreadCount != 1 || bobThreads[0].AttachmentCount != 1 {
		t.Fatalf("Bob's threads = %+v", bobThreads)
	}
	if subs := bobThreads[0].Subscribers; len(subs) != 1 || subs[0].Email != "alice@example.test" ||
		subs[0].Name != "Alice" {
		t.Errorf("Bob sees subscribers %+v, want Alice by name", subs)
	}
	bobThread := bobThreads[0].ThreadID

	// The sender's own copy is read; the recipient's is not.
	aliceThreads, err := f.svc.ListThreads(f.ctx, alice, aliceInbox, "")
	if err != nil || len(aliceThreads) != 1 || aliceThreads[0].UnreadCount != 0 {
		t.Fatalf("Alice's threads = %+v, %v", aliceThreads, err)
	}

	f.now = f.now.Add(time.Hour)
	if _, err := f.svc.AddMessage(f.ctx, bob, bobThread, "<p>Thanks, looks final.</p>", nil); err != nil {
		t.Fatal(err)
	}
	messages, err := f.svc.ListMessages(f.ctx, alice, threadID, "")
	if err != nil || len(messages) != 2 {
		t.Fatalf("ListMessages = %d, %v", len(messages), err)
	}
	if messages[0].SenderEmail != "bob@example.test" || messages[0].ReadAt != nil {
		t.Errorf("newest message = %+v, want Bob's, unread", messages[0])
	}
	if messages[0].SentAt != "2026-10-20T10:00:00+00:00" {
		t.Errorf("sent at %s, want the clock's time", messages[0].SentAt)
	}
	if len(messages[1].Attachments) != 1 || messages[1].Attachments[0].FileName != "q3.csv" {
		t.Errorf("first message attachments = %+v", messages[1].Attachments)
	}

	mailboxes, err := f.svc.ListMailboxes(f.ctx, alice, "")
	if err != nil || len(mailboxes) != 1 || mailboxes[0].UnreadCount != 1 || !mailboxes[0].IsDefault {
		t.Fatalf("ListMailboxes = %+v, %v", mailboxes, err)
	}
	folders, err := f.svc.ListFolders(f.ctx, alice, aliceMailbox, "")
	if err != nil || len(folders) != 1 || folders[0].UnreadCount != 1 || !folders[0].IsDefault {
		t.Fatalf("ListFolders = %+v, %v", folders, err)
	}

	// Search runs through the engine's own index.
	for _, tc := range []struct {
		search  string
		threads int
	}{{"quarterly", 1}, {"final", 1}, {"+numbers +final", 0}, {"bob", 1}, {"absent", 0}} {
		threads, err := f.svc.ListThreads(f.ctx, alice, aliceInbox, tc.search)
		if err != nil {
			t.Fatalf("search %q: %v", tc.search, err)
		}
		if len(threads) != tc.threads {
			t.Errorf("search %q found %d threads, want %d", tc.search, len(threads), tc.threads)
		}
		// A search result keeps its read state; the search narrows its
		// message count instead.
		for _, th := range threads {
			if th.UnreadCount != 1 {
				t.Errorf("search %q reports %d unread, want Bob's reply", tc.search, th.UnreadCount)
			}
		}
		mailboxes, err := f.svc.ListMailboxes(f.ctx, alice, tc.search)
		if err != nil {
			t.Fatalf("search %q in mailboxes: %v", tc.search, err)
		}
		if got := mailboxes[0].ThreadCount; got != int64(tc.threads) {
			t.Errorf("search %q counts %d threads in the mailbox, want %d", tc.search, got, tc.threads)
		}
		if got := mailboxes[0].UnreadCount; got != int64(tc.threads) {
			t.Errorf("search %q counts %d unread in the mailbox, want %d", tc.search, got, tc.threads)
		}
		folders, err := f.svc.ListFolders(f.ctx, alice, aliceMailbox, tc.search)
		if err != nil {
			t.Fatalf("search %q in folders: %v", tc.search, err)
		}
		if got := folders[0].ThreadCount; got != int64(tc.threads) {
			t.Errorf("search %q counts %d threads in the folder, want %d", tc.search, got, tc.threads)
		}
		if got := folders[0].UnreadCount; got != int64(tc.threads) {
			t.Errorf("search %q counts %d unread in the folder, want %d", tc.search, got, tc.threads)
		}
	}
	found, err := f.svc.ListMessages(f.ctx, alice, threadID, "numbers")
	if err != nil || len(found) != 1 {
		t.Fatalf("message search = %d, %v", len(found), err)
	}

	if err := f.svc.MarkThreadRead(f.ctx, f.db, alice, threadID); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.svc.MarkMessageRead(f.ctx, bob, messages[0].MessageID); err != nil || !ok {
		t.Fatalf("MarkMessageRead = %v, %v", ok, err)
	}
	if mailboxes, _ := f.svc.ListMailboxes(f.ctx, alice, ""); mailboxes[0].UnreadCount != 0 {
		t.Errorf("Alice has %d unread after reading", mailboxes[0].UnreadCount)
	}
	results, err := f.svc.ListThreads(f.ctx, alice, aliceInbox, "final")
	if err != nil || len(results) != 1 {
		t.Fatalf("search after reading = %d, %v", len(results), err)
	}
	if results[0].UnreadCount != 0 || results[0].MessageCount != 1 {
		t.Errorf("read thread in search = %d unread of %d messages, want 0 of the 1 matching",
			results[0].UnreadCount, results[0].MessageCount)
	}
	if listed, _ := f.svc.ListThreads(f.ctx, alice, aliceInbox, ""); listed[0].MessageCount != 2 {
		t.Errorf("listing reports %d messages, want 2", listed[0].MessageCount)
	}
	// The read thread still matches, but no longer counts as unread.
	if mailboxes, _ := f.svc.ListMailboxes(f.ctx, alice, "final"); mailboxes[0].ThreadCount != 1 ||
		mailboxes[0].UnreadCount != 0 {
		t.Errorf("mailbox in search = %d unread of %d threads, want 0 of 1",
			mailboxes[0].UnreadCount, mailboxes[0].ThreadCount)
	}
	if folders, _ := f.svc.ListFolders(f.ctx, alice, aliceMailbox, "final"); folders[0].ThreadCount != 1 ||
		folders[0].UnreadCount != 0 {
		t.Errorf("folder in search = %d unread of %d threads, want 0 of 1",
			folders[0].UnreadCount, folders[0].ThreadCount)
	}

	attachmentID := messages[1].Attachments[0].AttachmentID
	data, err := f.svc.GetAttachment(f.ctx, alice, attachmentID)
	if err != nil || data == nil || string(data.FileData) != "a,b\n1,2\n" {
		t.Fatalf("GetAttachment = %+v, %v", data, err)
	}
	if ok, err := f.svc.DeleteAttachment(f.ctx, alice, attachmentID); err != nil || !ok {
		t.Fatalf("DeleteAttachment = %v, %v", ok, err)
	}
	if ok, _ := f.svc.DeleteAttachment(f.ctx, alice, attachmentID); ok {
		t.Error("an attachment was deleted twice")
	}

	// Adding a subscriber posts a message to everyone, including the new one.
	carol, _, carolInbox := f.account("carol@example.test", "Carol")
	if _, err := f.svc.AddSubscriber(f.ctx, alice, threadID, "carol@example.test"); err != nil {
		t.Fatal(err)
	}
	if threads, _ := f.svc.ListThreads(f.ctx, carol, carolInbox, ""); len(threads) != 1 {
		t.Errorf("Carol has %d threads after being added", len(threads))
	}
	if n := f.count(`SELECT COUNT(*) FROM "g_subscription" WHERE "email" = ?`, "carol@example.test"); n != 1 {
		t.Errorf("Carol is subscribed %d times", n)
	}

	if ok, err := f.svc.DeleteThread(f.ctx, bob, bobThread); err != nil || !ok {
		t.Fatalf("DeleteThread = %v, %v", ok, err)
	}
	if threads, _ := f.svc.ListThreads(f.ctx, bob, bobInbox, ""); len(threads) != 0 {
		t.Errorf("Bob still lists %d threads", len(threads))
	}
	if mailboxes, _ := f.svc.ListMailboxes(f.ctx, bob, ""); mailboxes[0].ThreadCount != 0 {
		t.Errorf("Bob's mailbox still counts %d threads", mailboxes[0].ThreadCount)
	}
	if threads, _ := f.svc.ListThreads(f.ctx, alice, aliceInbox, ""); len(threads) != 1 {
		t.Error("deleting Bob's copy removed Alice's")
	}
}

// With federation off, a recipient outside this Server's Domains is onboarded
// with a customer of their own, a mailbox that belongs to that customer, and a
// generated address on this Server's Domain. That is how 103mail.com reaches
// people elsewhere until it is a Registrar.
func TestOnboarding(t *testing.T) {
	f := newFixture(t)
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")
	// A customer with no user, so customer and user ids no longer line up.
	if _, err := store.Insert(f.ctx, f.db, `INSERT INTO "customer" ("name") VALUES (?)`, "spare"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"dave@elsewhere.test"}, "Hello", "<p>Hi</p>", nil); err != nil {
		t.Fatal(err)
	}
	var customerID, userCustomerID int64
	if err := f.db.QueryRowContext(f.ctx,
		`SELECT "alias"."customer_id", "user"."customer_id" FROM "alias" `+
			`JOIN "user" ON "user"."username" = "alias"."email" WHERE "alias"."email" = ?`,
		"dave@elsewhere.test").Scan(&customerID, &userCustomerID); err != nil {
		t.Fatal(err)
	}
	if customerID != userCustomerID {
		t.Errorf("Dave's mailbox belongs to customer %d, his user to %d", customerID, userCustomerID)
	}
	if n := f.count(`SELECT COUNT(*) FROM "thread" WHERE "customer_id" = ?`, customerID); n != 1 {
		t.Errorf("Dave has %d threads", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM "alias" WHERE "customer_id" = ? AND "email" LIKE '%@example.test'`,
		customerID); n != 1 {
		t.Errorf("Dave has %d generated addresses on this Server's Domain, want 1", n)
	}
	var validUntil time.Time
	if err := f.db.QueryRowContext(f.ctx, `SELECT "valid_until" FROM "token" WHERE "name" = 'Invite'`).
		Scan(&validUntil); err != nil {
		t.Fatal(err)
	}
	if want := f.now.AddDate(0, 0, inviteValidDays); !validUntil.Equal(want) {
		t.Errorf("invite valid until %v, want %v", validUntil, want)
	}
}

// notified records what was handed to the notifier.
type notified struct {
	to       string
	template string
	params   map[string]string
}

type fakeNotifier struct{ sent []notified }

func (n *fakeNotifier) Send(ctx context.Context, customerID *int64, toName, toEmail, templateName,
	preferredLanguage string, params map[string]string, bccEmail string) error {
	n.sent = append(n.sent, notified{to: toEmail, template: templateName, params: params})
	return nil
}

func TestInvitationCarriesALink(t *testing.T) {
	f := newFixture(t)
	notifier := &fakeNotifier{}
	f.svc = New(f.db, blob.New(t.TempDir()), notifier, nil, nil)
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")

	if _, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"dave@elsewhere.test"}, "Hello", "<p>Hi</p>", nil); err != nil {
		t.Fatal(err)
	}
	if len(notifier.sent) != 1 || notifier.sent[0].to != "dave@elsewhere.test" ||
		notifier.sent[0].template != InviteTemplate {
		t.Fatalf("notifications = %+v, want one invitation to Dave", notifier.sent)
	}
	var token string
	if err := f.db.QueryRowContext(f.ctx, `SELECT "value" FROM "token" WHERE "name" = 'Invite'`).
		Scan(&token); err != nil {
		t.Fatal(err)
	}
	// The link is on the Domain Dave's new account lives on, not on his own
	// address's domain, and carries nothing but the token (ADR 6).
	if want := "https://webmail.example.test/invite/" + token; notifier.sent[0].params["link"] != want {
		t.Errorf("link = %q, want %q", notifier.sent[0].params["link"], want)
	}
}

// Writing to an address on this Server's own Domain that has no account is
// refused before anything is written, naming the address.
func TestUnknownLocalRecipientsAreRefused(t *testing.T) {
	f := newFixture(t)
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")
	bob, _, bobInbox := f.account("bob@example.test", "Bob")

	_, err := f.svc.AddThread(f.ctx, alice, aliceInbox,
		[]string{"bob@example.test", "Nobody@Example.test", "ghost@example.test"}, "Hello", "<p>Hi</p>", nil)
	var unknown *UnknownRecipientsError
	if !errors.As(err, &unknown) || !slices.Equal(unknown.Addresses, []string{"nobody@example.test", "ghost@example.test"}) {
		t.Fatalf("AddThread = %v, want the two unknown addresses refused", err)
	}
	if threads, _ := f.svc.ListThreads(f.ctx, alice, aliceInbox, ""); len(threads) != 0 {
		t.Errorf("Alice has %d threads after a refused send", len(threads))
	}
	if threads, _ := f.svc.ListThreads(f.ctx, bob, bobInbox, ""); len(threads) != 0 {
		t.Errorf("Bob got %d threads from a refused send", len(threads))
	}
	if n := f.count(`SELECT COUNT(*) FROM "user"`); n != 0 {
		t.Errorf("%d accounts were created", n)
	}

	threadID, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"bob@example.test"}, "Hello", "<p>Hi</p>", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AddSubscriber(f.ctx, alice, threadID, "ghost@example.test"); !errors.As(err, &unknown) {
		t.Errorf("AddSubscriber = %v, want the unknown address refused", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM "g_subscription" WHERE "email" = 'ghost@example.test'`); n != 0 {
		t.Error("the unknown address was subscribed")
	}
}

// An incoming envelope for an address on this Domain that has no account
// delivers nothing and creates nothing, so the inbound endpoint answers 404.
func TestIncomingMailIsRefusedForUnknownAddresses(t *testing.T) {
	f := newFixture(t)
	delivered, refused, err := f.svc.DeliverInbound(f.ctx, federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
		Subject: "Hi", MessageFedID: "0192f0c0-0000-7000-8000-000000000002",
		SenderEmail: "pete@peer.test", SenderName: "Pete", Body: "Anyone?", SentAt: f.now,
		Recipients: []string{"nobody@example.test"},
	})
	if err != nil || delivered != 0 || !slices.Equal(refused, []string{"nobody@example.test"}) {
		t.Fatalf("DeliverInbound = %d, %v, %v, want nothing delivered and Nobody refused", delivered, refused, err)
	}
	if n := f.count(`SELECT COUNT(*) FROM "user"`); n != 0 {
		t.Errorf("%d accounts were created", n)
	}
}

// An envelope for one known and one unknown address is delivered to the known
// one, and the unknown one is reported so the sender's Domain can bounce it
// (RFC 6.3). A replay reports the same, and delivers nothing twice.
func TestPartlyUnknownEnvelopeReportsTheUnknown(t *testing.T) {
	f := newFixture(t)
	bob, _, bobInbox := f.account("bob@example.test", "Bob")
	in := federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
		Subject: "Hi", MessageFedID: "0192f0c0-0000-7000-8000-000000000002",
		SenderEmail: "pete@peer.test", SenderName: "Pete", Body: "Anyone?", SentAt: f.now,
		Recipients: []string{"bob@example.test", "nobody@example.test"},
	}
	for attempt := 1; attempt <= 2; attempt++ {
		delivered, refused, err := f.svc.DeliverInbound(f.ctx, in)
		if err != nil || delivered != 1 || !slices.Equal(refused, []string{"nobody@example.test"}) {
			t.Fatalf("attempt %d: DeliverInbound = %d, %v, %v", attempt, delivered, refused, err)
		}
		undelivered, err := f.svc.Undelivered(f.ctx, in)
		if err != nil || !slices.Equal(undelivered, []string{"nobody@example.test"}) {
			t.Fatalf("attempt %d: Undelivered = %v, %v", attempt, undelivered, err)
		}
	}
	threads, err := f.svc.ListThreads(f.ctx, bob, bobInbox, "")
	if err != nil || len(threads) != 1 {
		t.Fatalf("Bob has %d threads, %v", len(threads), err)
	}
	// Pete's message and the notice that nobody@example.test does not exist,
	// each once, however often the envelope is replayed.
	if messages, _ := f.svc.ListMessages(f.ctx, bob, threads[0].ThreadID, ""); len(messages) != 2 {
		t.Errorf("Bob has %d messages after a replay, want Pete's and the refusal notice", len(messages))
	}
}

// The thread's members come from the envelope, so a reply written here reaches
// members on Domains that were not among the recipients (RFC 5.3).
func TestIncomingMembersBecomeSubscribers(t *testing.T) {
	f := newFixture(t)
	bob, _, bobInbox := f.account("bob@example.test", "Bob")
	delivered, _, err := f.svc.DeliverInbound(f.ctx, federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
		Subject: "Three Domains", MessageFedID: "0192f0c0-0000-7000-8000-000000000002",
		SenderEmail: "pete@peer.test", SenderName: "Pete", Body: "Hi all", SentAt: f.now,
		Recipients: []string{"bob@example.test"},
		Members:    []string{"pete@peer.test", "bob@example.test", "erin@third.test"},
	})
	if err != nil || delivered != 1 {
		t.Fatalf("DeliverInbound = %d, %v", delivered, err)
	}
	threads, err := f.svc.ListThreads(f.ctx, bob, bobInbox, "")
	if err != nil || len(threads) != 1 {
		t.Fatalf("Bob has %d threads, %v", len(threads), err)
	}
	var subscribers []string
	for _, p := range threads[0].Subscribers {
		subscribers = append(subscribers, p.Email)
	}
	slices.Sort(subscribers)
	if !slices.Equal(subscribers, []string{"erin@third.test", "pete@peer.test"}) {
		t.Errorf("Bob sees subscribers %v, want Pete and Erin", subscribers)
	}
}

// Every Domain shows a thread in one order: by send time in whole seconds,
// then by federation id, not by when each message arrived (RFC 5.5).
func TestMessagesAreOrderedBySendTime(t *testing.T) {
	f := newFixture(t)
	bob, _, bobInbox := f.account("bob@example.test", "Bob")
	inbound := func(messageFedID string, sentAt time.Time, body string) {
		t.Helper()
		if _, _, err := f.svc.DeliverInbound(f.ctx, federation.Inbound{
			OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
			Subject: "Order", MessageFedID: messageFedID,
			SenderEmail: "pete@peer.test", SenderName: "Pete", Body: body, SentAt: sentAt,
			Recipients: []string{"bob@example.test"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Pete's second message reaches Bob first, and two of his messages share
	// a second and arrive in reverse order.
	inbound("0192f0c0-0000-7000-8000-000000000020", f.now.Add(20*time.Second), "third")
	inbound("0192f0c0-0000-7000-8000-000000000003", f.now, "second, same second as first")
	inbound("0192f0c0-0000-7000-8000-000000000002", f.now, "first")
	threads, err := f.svc.ListThreads(f.ctx, bob, bobInbox, "")
	if err != nil || len(threads) != 1 {
		t.Fatalf("Bob has %d threads, %v", len(threads), err)
	}
	// Bob replies with his clock between Pete's messages.
	f.now = f.now.Add(10 * time.Second)
	if _, err := f.svc.AddMessage(f.ctx, bob, threads[0].ThreadID, "<p>reply</p>", nil); err != nil {
		t.Fatal(err)
	}
	messages, err := f.svc.ListMessages(f.ctx, bob, threads[0].ThreadID, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i := len(messages) - 1; i >= 0; i-- { // the API lists newest first
		got = append(got, strings.TrimSpace(messages[i].Body))
	}
	want := []string{"first", "second, same second as first", "reply", "third"}
	if !slices.Equal(got, want) {
		t.Errorf("Bob reads %q, want %q", got, want)
	}
	if n := f.count(`SELECT COUNT(*) FROM "g_message" WHERE "federation_id" IS NULL`); n != 0 {
		t.Errorf("%d messages have no federation id; it is assigned when a message is written", n)
	}
}

// An address the peer refused as unknown is not a member of the thread: it is
// marked, not deleted, left out of the member list, and later messages do not
// go to it, so the sender is bounced once (RFC 6.5).
func TestRefusedRecipientsLeaveTheThread(t *testing.T) {
	f := federatedFixture(t)
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")
	threadID, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"bob@peer.test", "nobody@peer.test"},
		"Invitation", "<p>Come</p>", nil)
	if err != nil {
		t.Fatal(err)
	}
	threadFedID, err := store.SelectString(f.ctx, f.db, `SELECT "federation_id" FROM "g_thread" LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Bounce(f.ctx, federation.BounceNotice{
		OriginDomain: "example.test", SenderEmail: "alice@example.test", Subject: "Invitation",
		ThreadFedID: threadFedID, Recipients: []string{"nobody@peer.test"}, PeerDomain: "peer.test",
		EnvelopeID: "e", HTTPStatus: 202, ErrorCode: federation.ErrUnknownRecipient, Refused: true,
	}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT COUNT(*) FROM "g_subscription" WHERE "email" = 'nobody@peer.test' AND "refused_at" IS NOT NULL`); n != 1 {
		t.Errorf("%d refused subscriptions for nobody@peer.test, want it marked", n)
	}
	threads, err := f.svc.ListThreads(f.ctx, alice, aliceInbox, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range threads {
		if th.ThreadID != threadID {
			continue
		}
		for _, p := range th.Subscribers {
			if p.Email == "nobody@peer.test" {
				t.Error("Alice still sees nobody@peer.test as a member")
			}
		}
	}

	f.exec(`DELETE FROM "federation_outbox"`)
	if _, err := f.svc.AddMessage(f.ctx, alice, threadID, "<p>Anyone else?</p>", nil); err != nil {
		t.Fatal(err)
	}
	body, err := store.SelectString(f.ctx, f.db, `SELECT "body" FROM "federation_outbox" WHERE "peer_domain" = 'peer.test'`)
	if err != nil {
		t.Fatal(err)
	}
	var envelope federation.Envelope
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(envelope.Recipients, "nobody@peer.test") || slices.Contains(envelope.Thread.Members, "nobody@peer.test") {
		t.Errorf("the reply still names nobody@peer.test: recipients %v, members %v",
			envelope.Recipients, envelope.Thread.Members)
	}
}

// Refusing an address on its own Domain, a Domain tells the thread's other
// Domains the way an invitation is told: a message in the thread from its
// mailer-daemon, with the address refused (RFC 6.5).
func TestARefusalIsToldToTheThread(t *testing.T) {
	f := federatedFixture(t)
	bob, _, bobInbox := f.account("bob@example.test", "Bob")
	_, refused, err := f.svc.DeliverInbound(f.ctx, federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
		Subject: "Invitation", MessageFedID: "0192f0c0-0000-7000-8000-000000000002",
		SenderEmail: "pete@peer.test", SenderName: "Pete", Body: "Come", SentAt: f.now,
		Recipients: []string{"bob@example.test", "nobody@example.test"},
		Members:    []string{"pete@peer.test", "bob@example.test", "nobody@example.test", "erin@third.test"},
	})
	if err != nil || !slices.Equal(refused, []string{"nobody@example.test"}) {
		t.Fatalf("DeliverInbound refused %v, %v", refused, err)
	}

	threads, err := f.svc.ListThreads(f.ctx, bob, bobInbox, "")
	if err != nil || len(threads) != 1 {
		t.Fatalf("Bob has %d threads, %v", len(threads), err)
	}
	messages, err := f.svc.ListMessages(f.ctx, bob, threads[0].ThreadID, "")
	if err != nil || len(messages) != 2 {
		t.Fatalf("Bob has %d messages, want Pete's and the notice: %v", len(messages), err)
	}
	if notice := messages[0]; notice.SenderEmail != "mailer-daemon@example.test" ||
		!strings.Contains(notice.Body, "nobody@example.test") {
		t.Errorf("the notice = %+v", notice)
	}

	for peer, recipients := range map[string][]string{
		"peer.test":  {"pete@peer.test"},
		"third.test": {"erin@third.test"},
	} {
		body, err := store.SelectString(f.ctx, f.db,
			`SELECT "body" FROM "federation_outbox" WHERE "peer_domain" = ?`, peer)
		if err != nil {
			t.Fatal(err)
		}
		var envelope federation.Envelope
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Message.SenderEmail != "mailer-daemon@example.test" ||
			!slices.Equal(envelope.Recipients, recipients) ||
			!slices.Equal(envelope.Thread.Refused, []string{"nobody@example.test"}) ||
			slices.Contains(envelope.Thread.Members, "nobody@example.test") ||
			slices.Contains(envelope.Thread.Members, "mailer-daemon@example.test") {
			t.Errorf("the notice to %s = %+v", peer, envelope)
		}
	}
}

// A notice can only refuse addresses on its sender's own domain, and the
// daemon that sends it is never recorded as a member (RFC 5.3, 6.5).
func TestARefusalOnlyCountsForTheSendersOwnAddresses(t *testing.T) {
	f := newFixture(t)
	bob, _, bobInbox := f.account("bob@example.test", "Bob")
	members := []string{"pete@peer.test", "bob@example.test", "nobody@peer.test", "gail@third.test"}
	if _, _, err := f.svc.DeliverInbound(f.ctx, federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
		Subject: "Party", MessageFedID: "0192f0c0-0000-7000-8000-000000000002",
		SenderEmail: "pete@peer.test", SenderName: "Pete", Body: "Come", SentAt: f.now,
		Recipients: []string{"bob@example.test"}, Members: members,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.DeliverInbound(f.ctx, federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
		Subject: "Party", MessageFedID: "0192f0c0-0000-7000-8000-000000000003",
		SenderEmail: "mailer-daemon@peer.test", SenderName: "Mailer Daemon",
		Body: "nobody@peer.test does not exist", SentAt: f.now,
		Recipients: []string{"bob@example.test"},
		Members:    []string{"pete@peer.test", "bob@example.test", "gail@third.test"},
		Refused:    []string{"nobody@peer.test", "gail@third.test"},
	}); err != nil {
		t.Fatal(err)
	}
	threads, err := f.svc.ListThreads(f.ctx, bob, bobInbox, "")
	if err != nil || len(threads) != 1 {
		t.Fatalf("Bob has %d threads, %v", len(threads), err)
	}
	var subscribers []string
	for _, p := range threads[0].Subscribers {
		subscribers = append(subscribers, p.Email)
	}
	slices.Sort(subscribers)
	if want := []string{"gail@third.test", "pete@peer.test"}; !slices.Equal(subscribers, want) {
		t.Errorf("Bob sees subscribers %v, want %v", subscribers, want)
	}
}

// A new member is shown the thread's earlier messages, on this Server or when
// an invitation arrives from another Domain (RFC 5.6).
func TestAnInvitationBringsTheHistory(t *testing.T) {
	f := newFixture(t)
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")
	bob, _, bobInbox := f.account("bob@example.test", "Bob")
	chris, _, chrisInbox := f.account("chris@example.test", "Chris")
	dave, _, daveInbox := f.account("dave@example.test", "Dave")

	if _, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"bob@example.test"}, "Vendors", "<p>Shortlist</p>", nil); err != nil {
		t.Fatal(err)
	}
	bobThreads, _ := f.svc.ListThreads(f.ctx, bob, bobInbox, "")
	if _, err := f.svc.AddMessage(f.ctx, bob, bobThreads[0].ThreadID, "<p>Vendor B</p>", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AddSubscriber(f.ctx, bob, bobThreads[0].ThreadID, "chris@example.test"); err != nil {
		t.Fatal(err)
	}
	threads, _ := f.svc.ListThreads(f.ctx, chris, chrisInbox, "")
	if len(threads) != 1 {
		t.Fatalf("Chris has %d threads", len(threads))
	}
	messages, _ := f.svc.ListMessages(f.ctx, chris, threads[0].ThreadID, "")
	if len(messages) != 3 || strings.TrimSpace(messages[2].Body) != "Shortlist" {
		t.Errorf("Chris has %d messages, the oldest %q; want the two earlier ones and the invitation", len(messages),
			strings.TrimSpace(messages[len(messages)-1].Body))
	}

	// From another Domain: Pete's thread with Bob, then an invitation for Dave.
	in := federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
		Subject: "Across", MessageFedID: "0192f0c0-0000-7000-8000-000000000002",
		SenderEmail: "pete@peer.test", SenderName: "Pete", Body: "First", SentAt: f.now,
		Recipients: []string{"bob@example.test"}, Members: []string{"pete@peer.test", "bob@example.test"},
	}
	if _, _, err := f.svc.DeliverInbound(f.ctx, in); err != nil {
		t.Fatal(err)
	}
	in.MessageFedID, in.Body = "0192f0c0-0000-7000-8000-000000000003", "Pete added Dave"
	in.Recipients = []string{"bob@example.test", "dave@example.test"}
	in.Members = []string{"pete@peer.test", "bob@example.test", "dave@example.test"}
	if _, _, err := f.svc.DeliverInbound(f.ctx, in); err != nil {
		t.Fatal(err)
	}
	daveThreads, _ := f.svc.ListThreads(f.ctx, dave, daveInbox, "")
	if len(daveThreads) != 1 {
		t.Fatalf("Dave has %d threads", len(daveThreads))
	}
	if messages, _ := f.svc.ListMessages(f.ctx, dave, daveThreads[0].ThreadID, ""); len(messages) != 2 {
		t.Errorf("Dave has %d messages, want Pete's first and the invitation", len(messages))
	}
}

// Leaving is told to the thread by the leaver's own Domain, in their name.
// After that they get nothing new and cannot write; nobody can remove anyone
// else (RFC 5.6).
func TestLeavingAThread(t *testing.T) {
	f := federatedFixture(t)
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")
	chris, _, chrisInbox := f.account("chris@example.test", "Chris")
	threadID, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"chris@example.test", "bob@peer.test"},
		"Vendors", "<p>Shortlist</p>", nil)
	if err != nil {
		t.Fatal(err)
	}
	chrisThreads, _ := f.svc.ListThreads(f.ctx, chris, chrisInbox, "")
	chrisThread := chrisThreads[0].ThreadID

	var notYours *OnlyYourselfError
	if _, err := f.svc.Leave(f.ctx, alice, threadID, "chris@example.test"); !errors.As(err, &notYours) {
		t.Fatalf("Alice removing Chris = %v, want refused", err)
	}

	f.exec(`DELETE FROM "federation_outbox"`)
	if _, err := f.svc.Leave(f.ctx, chris, chrisThread, "chris@example.test"); err != nil {
		t.Fatal(err)
	}
	body, err := store.SelectString(f.ctx, f.db, `SELECT "body" FROM "federation_outbox" WHERE "peer_domain" = 'peer.test'`)
	if err != nil {
		t.Fatal(err)
	}
	var notice federation.Envelope
	if err := json.Unmarshal([]byte(body), &notice); err != nil {
		t.Fatal(err)
	}
	if notice.Message.SenderEmail != "chris@example.test" || !slices.Equal(notice.Thread.Left, []string{"chris@example.test"}) ||
		slices.Contains(notice.Thread.Members, "chris@example.test") {
		t.Errorf("the notice = %+v", notice)
	}
	aliceThreads, _ := f.svc.ListThreads(f.ctx, alice, aliceInbox, "")
	for _, p := range aliceThreads[0].Subscribers {
		if p.Email == "chris@example.test" {
			t.Error("Alice still sees Chris as a member")
		}
	}

	before, _ := f.svc.ListMessages(f.ctx, chris, chrisThread, "")
	if _, err := f.svc.AddMessage(f.ctx, alice, threadID, "<p>Going with B</p>", nil); err != nil {
		t.Fatal(err)
	}
	if after, _ := f.svc.ListMessages(f.ctx, chris, chrisThread, ""); len(after) != len(before) {
		t.Errorf("Chris got %d new messages after leaving", len(after)-len(before))
	}
	var notMember *NotMemberError
	if _, err := f.svc.AddMessage(f.ctx, chris, chrisThread, "<p>Wait</p>", nil); !errors.As(err, &notMember) {
		t.Errorf("Chris replying after leaving = %v, want refused", err)
	}
}

// A member who left gets nothing more, even from an envelope written before
// their Domain's notice reached the sender. They are not refused either:
// they exist, they just left (RFC 5.6).
func TestADepartedMemberIsNotDeliveredTo(t *testing.T) {
	f := federatedFixture(t)
	chris, _, chrisInbox := f.account("chris@example.test", "Chris")
	in := federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
		Subject: "Vendors", MessageFedID: "0192f0c0-0000-7000-8000-000000000002",
		SenderEmail: "pete@peer.test", SenderName: "Pete", Body: "First", SentAt: f.now,
		Recipients: []string{"chris@example.test"}, Members: []string{"pete@peer.test", "chris@example.test"},
	}
	if _, _, err := f.svc.DeliverInbound(f.ctx, in); err != nil {
		t.Fatal(err)
	}
	threads, _ := f.svc.ListThreads(f.ctx, chris, chrisInbox, "")
	if _, err := f.svc.Leave(f.ctx, chris, threads[0].ThreadID, "chris@example.test"); err != nil {
		t.Fatal(err)
	}
	in.MessageFedID, in.Body = "0192f0c0-0000-7000-8000-000000000003", "Written before the notice arrived"
	delivered, refused, err := f.svc.DeliverInbound(f.ctx, in)
	if err != nil || delivered != 0 || len(refused) != 0 {
		t.Errorf("DeliverInbound = %d, %v, %v; want nothing delivered and nobody refused", delivered, refused, err)
	}
	if messages, _ := f.svc.ListMessages(f.ctx, chris, threads[0].ThreadID, ""); len(messages) != 1 {
		t.Errorf("Chris has %d messages after leaving, want only the first", len(messages))
	}
}

// A departure only counts for an address on the sender's own domain.
func TestALeaveOnlyCountsForTheSendersOwnAddresses(t *testing.T) {
	f := newFixture(t)
	bob, _, bobInbox := f.account("bob@example.test", "Bob")
	in := federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
		Subject: "Party", MessageFedID: "0192f0c0-0000-7000-8000-000000000002",
		SenderEmail: "pete@peer.test", SenderName: "Pete", Body: "Come", SentAt: f.now,
		Recipients: []string{"bob@example.test"},
		Members:    []string{"pete@peer.test", "bob@example.test", "gail@third.test"},
	}
	if _, _, err := f.svc.DeliverInbound(f.ctx, in); err != nil {
		t.Fatal(err)
	}
	in.MessageFedID, in.Body = "0192f0c0-0000-7000-8000-000000000003", "Pete left"
	in.Members = []string{"bob@example.test"}
	in.Left = []string{"pete@peer.test", "gail@third.test"}
	if _, _, err := f.svc.DeliverInbound(f.ctx, in); err != nil {
		t.Fatal(err)
	}
	threads, _ := f.svc.ListThreads(f.ctx, bob, bobInbox, "")
	var subscribers []string
	for _, p := range threads[0].Subscribers {
		subscribers = append(subscribers, p.Email)
	}
	if !slices.Equal(subscribers, []string{"gail@third.test"}) {
		t.Errorf("Bob sees subscribers %v, want only Gail", subscribers)
	}
}

// An unknown address on this Server's own Domain in an envelope's members is
// not recorded: it was refused, it is not on the thread (RFC 5.3).
func TestUnknownLocalMembersAreNotRecorded(t *testing.T) {
	f := newFixture(t)
	bob, _, bobInbox := f.account("bob@example.test", "Bob")
	if _, _, err := f.svc.DeliverInbound(f.ctx, federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
		Subject: "Party", MessageFedID: "0192f0c0-0000-7000-8000-000000000002",
		SenderEmail: "pete@peer.test", SenderName: "Pete", Body: "Come", SentAt: f.now,
		Recipients: []string{"bob@example.test", "ghost@example.test"},
		Members:    []string{"pete@peer.test", "bob@example.test", "ghost@example.test"},
	}); err != nil {
		t.Fatal(err)
	}
	threads, err := f.svc.ListThreads(f.ctx, bob, bobInbox, "")
	if err != nil || len(threads) != 1 {
		t.Fatalf("Bob has %d threads, %v", len(threads), err)
	}
	if subs := threads[0].Subscribers; len(subs) != 1 || subs[0].Email != "pete@peer.test" {
		t.Errorf("Bob sees subscribers %+v, want only Pete", subs)
	}
}

// A mailbox's generated address is on a Domain this Server hosts, even when
// the domain table also holds peers that federation discovered.
func TestGeneratedAddressesStayOnHostedDomains(t *testing.T) {
	f := newFixture(t)
	f.exec(`DELETE FROM "domain"`)
	f.exec(`INSERT INTO "domain" ("name", "is_local") VALUES ('peer.test', FALSE)`)
	f.exec(`INSERT INTO "domain" ("name", "is_local") VALUES ('example.test', TRUE)`)
	alice, _, _ := f.account("alice@example.test", "Alice")
	if n := f.count(`SELECT COUNT(*) FROM "alias" WHERE "customer_id" = ? AND "email" LIKE '%@peer.test'`, alice); n != 0 {
		t.Errorf("Alice got %d addresses on a peer's domain", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM "alias" WHERE "customer_id" = ? AND "email" LIKE '%@example.test'`, alice); n != 2 {
		t.Errorf("Alice has %d addresses on example.test, want hers and a generated one", n)
	}
}

func TestContacts(t *testing.T) {
	f := newFixture(t)
	alice, _, _ := f.account("alice@example.test", "Alice")
	id, err := f.svc.AddContact(f.ctx, alice, "Erin@Example.test", "Erin Smith")
	if err != nil || id == 0 {
		t.Fatalf("AddContact = %d, %v", id, err)
	}
	if again, _ := f.svc.AddContact(f.ctx, alice, "erin@example.test", "Erin"); again != id {
		t.Errorf("the same address in another case made contact %d, want %d", again, id)
	}
	if ok, err := f.svc.EditContact(f.ctx, alice, id, "", "Erin Jones"); err != nil || !ok {
		t.Fatalf("EditContact = %v, %v", ok, err)
	}
	for search, want := range map[string]int{"jones": 1, "erin@example.test": 1, "smith": 0, "example": 1} {
		contacts, err := f.svc.ListContacts(f.ctx, alice, search)
		if err != nil {
			t.Fatalf("search %q: %v", search, err)
		}
		if len(contacts) != want {
			t.Errorf("search %q found %d contacts, want %d", search, len(contacts), want)
		}
	}
	if ok, err := f.svc.DeleteContact(f.ctx, alice, id); err != nil || !ok {
		t.Fatalf("DeleteContact = %v, %v", ok, err)
	}
	if contacts, _ := f.svc.ListContacts(f.ctx, alice, ""); len(contacts) != 0 {
		t.Errorf("%d contacts after deleting the only one", len(contacts))
	}
}

func TestRecipientsBecomeContacts(t *testing.T) {
	f := newFixture(t)
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")
	bob, _, _ := f.account("bob@example.test", "Bob")
	carol, _, _ := f.account("carol@example.test", "Carol")

	if _, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"bob@example.test", "carol@example.test"},
		"Lunch", "<p>Noon?</p>", nil); err != nil {
		t.Fatal(err)
	}
	has := func(customerID int64, email, name string) bool {
		return f.count(`SELECT COUNT(*) FROM "contact" WHERE "customer_id" = ? AND "email" = ? AND "name" = ?`,
			customerID, email, name) == 1
	}
	// Writing to someone on this Domain makes them a contact, by name, the
	// same as writing to someone on another Domain does.
	if !has(alice, "bob@example.test", "Bob") || !has(alice, "carol@example.test", "Carol") {
		t.Error("Alice does not have the local recipients she wrote to as contacts")
	}
	if !has(bob, "alice@example.test", "Alice") || !has(carol, "alice@example.test", "Alice") {
		t.Error("the recipients do not have Alice as a contact")
	}
	// Being on the same thread is not an exchange: Bob and Carol have not
	// written to each other.
	if has(bob, "carol@example.test", "Carol") || has(carol, "bob@example.test", "Bob") {
		t.Error("co-recipients became each other's contacts")
	}
}

func TestFoldersAndMailboxes(t *testing.T) {
	f := newFixture(t)
	alice, mailbox, inbox := f.account("alice@example.test", "Alice")
	archive, err := f.svc.AddFolder(f.ctx, alice, mailbox, "Archive")
	if err != nil || archive == 0 {
		t.Fatalf("AddFolder = %d, %v", archive, err)
	}
	threadID, err := f.svc.AddThread(f.ctx, alice, inbox, nil, "Note to self", "<p>x</p>", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.svc.EditThread(f.ctx, alice, threadID, archive); err != nil || !ok {
		t.Fatalf("EditThread = %v, %v", ok, err)
	}
	if ok, err := f.svc.EditFolder(f.ctx, alice, archive, "Old", false); err != nil || !ok {
		t.Fatalf("EditFolder = %v, %v", ok, err)
	}
	if ok, _ := f.svc.DeleteFolder(f.ctx, alice, inbox); ok {
		t.Error("the default folder was deleted")
	}
	if ok, err := f.svc.DeleteFolder(f.ctx, alice, archive); err != nil || !ok {
		t.Fatalf("DeleteFolder = %v, %v", ok, err)
	}
	if threads, _ := f.svc.ListThreads(f.ctx, alice, inbox, ""); len(threads) != 1 {
		t.Error("the thread did not move back to the inbox")
	}

	second, err := f.svc.AddMailbox(f.ctx, alice, "Work", "blue", "", "Alice")
	if err != nil || second == 0 {
		t.Fatalf("AddMailbox = %d, %v", second, err)
	}
	shared := true
	if _, err := f.svc.EditMailbox(f.ctx, alice, 0, second, "Work", "green", nil, &shared, "", ""); err != nil {
		t.Fatal(err)
	}
	mailboxes, err := f.svc.ListMailboxes(f.ctx, alice, "")
	if err != nil || len(mailboxes) != 2 || !mailboxes[1].IsShared || mailboxes[1].Color != "green" {
		t.Fatalf("ListMailboxes = %+v, %v", mailboxes, err)
	}
	if ok, _ := f.svc.DeleteMailbox(f.ctx, alice, mailbox); ok {
		t.Error("the default mailbox was deleted")
	}
	if ok, err := f.svc.DeleteMailbox(f.ctx, alice, second); err != nil || !ok {
		t.Fatalf("DeleteMailbox = %v, %v", ok, err)
	}
	if domain, err := f.svc.DefaultDomain(f.ctx, f.db); err != nil || domain != "example.test" {
		t.Errorf("DefaultDomain = %q, %v", domain, err)
	}
}

func TestInbound(t *testing.T) {
	f := newFixture(t)
	bob, _, bobInbox := f.account("bob@example.test", "Bob")

	if seen, err := f.svc.SeenInbound(f.ctx, "ses-1"); err != nil || seen {
		t.Fatalf("first SeenInbound = %v, %v", seen, err)
	}
	if seen, err := f.svc.SeenInbound(f.ctx, "ses-1"); err != nil || !seen {
		t.Fatalf("second SeenInbound = %v, %v", seen, err)
	}

	gThreadID, err := f.svc.AddExternalThread(f.ctx, "Dana@Mail.TEST", "Dana",
		[]string{"BOB@example.test", "dana@mail.test"}, "From outside", "<p>Hello Bob</p>", nil)
	if err != nil || gThreadID == 0 {
		t.Fatalf("AddExternalThread = %d, %v", gThreadID, err)
	}
	if n := f.count(`SELECT COUNT(*) FROM "g_subscription" WHERE "g_thread_id" = ?`, gThreadID); n != 2 {
		t.Errorf("%d subscriptions, want the sender and Bob once each", n)
	}

	delivered, _, err := f.svc.DeliverInbound(f.ctx, federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
		Subject: "Federated", MessageFedID: "0192f0c0-0000-7000-8000-000000000002",
		SenderEmail: "Pete@peer.test", SenderName: "Pete", Body: "From a peer",
		SentAt:     f.now.Add(-time.Minute),
		Recipients: []string{"bob@example.test", "pete@peer.test"},
		Attachments: []federation.InboundAttachment{{FederationID: "0192f0c0-0000-7000-8000-000000000003",
			FileName: "a/b.txt", MimeType: "text/plain", Data: []byte("x")}},
	})
	// Pete has no account here, and an incoming message never creates one.
	if err != nil || delivered != 1 {
		t.Fatalf("DeliverInbound = %d, %v", delivered, err)
	}
	threads, err := f.svc.ListThreads(f.ctx, bob, bobInbox, "")
	if err != nil || len(threads) != 2 {
		t.Fatalf("Bob has %d threads, %v", len(threads), err)
	}
	if n := f.count(`SELECT COUNT(*) FROM "g_attachment" WHERE "file_name" = ?`, "a_b.txt"); n != 1 {
		t.Error("the attachment name was not sanitised")
	}

	if err := f.svc.Bounce(f.ctx, federation.BounceNotice{
		OriginDomain: "example.test", SenderEmail: "Bob@example.test", Subject: "Lost",
		Recipients: []string{"x@peer.test"}, PeerDomain: "peer.test", EnvelopeID: "e", HTTPStatus: 404,
		ErrorCode: "unknown_recipient",
	}); err != nil {
		t.Fatal(err)
	}
	if threads, _ := f.svc.ListThreads(f.ctx, bob, bobInbox, "delivery"); len(threads) != 1 {
		t.Errorf("Bob has %d bounce threads", len(threads))
	}
}

func TestTruncateCountsRunes(t *testing.T) {
	if got := truncate("ééé", 2); got != "éé" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abc", 5); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
}

// A message to another Domain is queued in the same transaction, and the
// thread and message get the federated ids a reply will correlate on.
// federatedFixture is a fixture whose Server hosts example.test with
// federation on, so addresses elsewhere are queued for their own Domains.
func federatedFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(dir, "federation.json")
	if err := os.WriteFile(cfgFile, []byte(`{"domains":[{"domain":"example.test","customerId":0,"privateKeyFile":"`+
		keyFile+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEBMAIL_FEDERATION_CONFIG", cfgFile)
	cfg, err := federation.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}

	f := newFixture(t)
	fed, err := federation.New(f.db, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.svc = New(f.db, blob.New(t.TempDir()), nil, fed, nil)
	fed.Attach(f.svc)
	return f
}

func TestRemoteRecipientsAreQueued(t *testing.T) {
	f := federatedFixture(t)
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")

	if _, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"Bob@Peer.test", "carol@peer.test", "dave@other.test"},
		"Across", "<p>Hi</p>", []NewAttachment{{FileName: "a.txt", MimeType: "text/plain", FileData: []byte("a")}}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT COUNT(*) FROM "federation_outbox" WHERE "peer_domain" = 'peer.test' AND "status" = 'pending'`); n != 1 {
		t.Errorf("%d envelopes queued for peer.test, want one for both recipients", n)
	}
	// Each envelope names only its Destination's recipients (RFC 5.3), and
	// every one carries the whole membership so replies reach everyone.
	for peer, want := range map[string][]string{
		"peer.test":  {"bob@peer.test", "carol@peer.test"},
		"other.test": {"dave@other.test"},
	} {
		body, err := store.SelectString(f.ctx, f.db,
			`SELECT "body" FROM "federation_outbox" WHERE "peer_domain" = ?`, peer)
		if err != nil {
			t.Fatal(err)
		}
		var envelope federation.Envelope
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatal(err)
		}
		// The envelope says when the message was written, which is the time
		// stored with it, so both Domains order it the same (RFC 5.5).
		if envelope.Message.SentAt != federation.FormatTimestamp(f.now) {
			t.Errorf("the envelope to %s says sent at %s, want %s", peer, envelope.Message.SentAt,
				federation.FormatTimestamp(f.now))
		}
		recipients := slices.Clone(envelope.Recipients)
		slices.Sort(recipients)
		if !slices.Equal(recipients, want) {
			t.Errorf("the envelope to %s names %v, want %v", peer, recipients, want)
		}
		members := slices.Clone(envelope.Thread.Members)
		slices.Sort(members)
		if !slices.Equal(members, []string{"alice@example.test", "bob@peer.test", "carol@peer.test", "dave@other.test"}) {
			t.Errorf("the envelope to %s has members %v", peer, members)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM "g_thread" WHERE "federation_id" IS NOT NULL`); n != 1 {
		t.Error("the thread has no federation id")
	}
	if n := f.count(`SELECT COUNT(*) FROM "g_attachment" WHERE "federation_id" IS NOT NULL`); n != 1 {
		t.Error("the attachment has no federation id")
	}
	if n := f.count(`SELECT COUNT(*) FROM "user"`); n != 0 {
		t.Errorf("%d accounts onboarded for remote recipients", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM "contact" WHERE "customer_id" = ? AND "email" IN (?, ?)`,
		alice, "bob@peer.test", "carol@peer.test"); n != 2 {
		t.Errorf("Alice has %d of the remote recipients as contacts, want both", n)
	}
}

// An address with an alias here is delivered here, even on a domain this
// Server does not host: the local match comes before any peer (design 15.5).
// The reply goes out as that alias, since it is the one on the thread.
func TestAnAliasHereIsDeliveredHere(t *testing.T) {
	f := federatedFixture(t)
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")
	maurits, mauritsMailbox, _ := f.account("maurits@example.test", "Maurits")
	f.exec(`INSERT INTO "alias" ("customer_id", "email", "name", "mailbox_id") VALUES (?,?,?,?)`,
		maurits, "maurits@vdschee.test", "Maurits", mauritsMailbox)

	if _, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"Maurits@vdschee.test"},
		"Hello", "<p>ddd</p>", nil); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT COUNT(*) FROM "federation_outbox"`); n != 0 {
		t.Errorf("%d envelopes queued for an address that lives here", n)
	}
	threadID := f.count(`SELECT "id" FROM "thread" WHERE "customer_id" = ?`, maurits)
	if threadID == 0 {
		t.Fatal("Maurits has no copy of the thread")
	}

	if _, err := f.svc.AddMessage(f.ctx, maurits, threadID, "<p>got it</p>", nil); err != nil {
		t.Fatalf("Maurits could not reply: %v", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM "g_message" WHERE "sender_email" = ?`, "maurits@vdschee.test"); n != 1 {
		t.Errorf("%d replies sent as the alias on the thread, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM "message" WHERE "customer_id" = ?`, alice); n != 2 {
		t.Errorf("Alice has %d messages, want both", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM "federation_outbox"`); n != 0 {
		t.Errorf("%d envelopes queued for the reply", n)
	}
}

// A bounce for a message that timed out tells the sender it will be sent
// again when the Domain is back (RFC 6.5).
func TestATimedOutBounceSaysItWillBeResent(t *testing.T) {
	f := newFixture(t)
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")
	if err := f.svc.Bounce(f.ctx, federation.BounceNotice{
		OriginDomain: "example.test", SenderEmail: "alice@example.test", Subject: "Report",
		Recipients: []string{"erin@peer.test"}, PeerDomain: "peer.test", EnvelopeID: "e",
		ErrorCode: "timed_out", Backfill: true,
	}); err != nil {
		t.Fatal(err)
	}
	threads, _ := f.svc.ListThreads(f.ctx, alice, aliceInbox, "")
	if len(threads) != 1 {
		t.Fatalf("Alice has %d threads", len(threads))
	}
	messages, _ := f.svc.ListMessages(f.ctx, alice, threads[0].ThreadID, "")
	if len(messages) != 1 || !strings.Contains(messages[0].Body, "will be sent again when peer.test can be reached") {
		t.Errorf("the bounce reads %q", messages[0].Body)
	}
}

// A peer cannot write into a thread held here for someone who is not on it,
// even by listing them in thread.members; a member of the thread still can
// (RFC 5.6).
func TestIncomingMailFromANonMemberIsRefused(t *testing.T) {
	f := newFixture(t)
	bob, _, bobInbox := f.account("bob@example.test", "Bob")
	thread := "0192f0c0-0000-7000-8000-000000000001"
	if _, _, err := f.svc.DeliverInbound(f.ctx, federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: thread, Subject: "Acquisition",
		MessageFedID: "0192f0c0-0000-7000-8000-000000000002", SenderEmail: "pete@peer.test",
		Body: "Kick-off", SentAt: f.now, Recipients: []string{"bob@example.test"},
	}); err != nil {
		t.Fatal(err)
	}

	_, _, err := f.svc.DeliverInbound(f.ctx, federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: thread, Subject: "Acquisition",
		MessageFedID: "0192f0c0-0000-7000-8000-000000000003", SenderEmail: "mallory@peer.test",
		Body: "Let me in", SentAt: f.now, Recipients: []string{"bob@example.test"},
		Members: []string{"pete@peer.test", "bob@example.test", "mallory@peer.test"},
	})
	if !errors.Is(err, federation.ErrNotMember) {
		t.Fatalf("DeliverInbound from a non-member = %v, want ErrNotMember", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM "g_subscription" WHERE "email" = 'mallory@peer.test'`); n != 0 {
		t.Error("the non-member was made a member")
	}

	delivered, _, err := f.svc.DeliverInbound(f.ctx, federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: thread, Subject: "Acquisition",
		MessageFedID: "0192f0c0-0000-7000-8000-000000000004", SenderEmail: "pete@peer.test",
		Body: "Numbers tomorrow", SentAt: f.now, Recipients: []string{"bob@example.test"},
	})
	if err != nil || delivered != 1 {
		t.Fatalf("DeliverInbound from a member = %d, %v", delivered, err)
	}
	threads, _ := f.svc.ListThreads(f.ctx, bob, bobInbox, "")
	if len(threads) != 1 {
		t.Fatalf("Bob has %d threads, want 1", len(threads))
	}
	messages, _ := f.svc.ListMessages(f.ctx, bob, threads[0].ThreadID, "")
	if len(messages) != 2 {
		t.Errorf("Bob has %d messages, want the two from Pete", len(messages))
	}
}

// A file over the Domain's attachment limit is refused before anything is
// written, naming the file and the limit (RFC 7).
func TestAttachmentsOverTheLimitAreRefused(t *testing.T) {
	f := newFixture(t)
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")
	f.account("bob@example.test", "Bob")
	raw := NewAttachment{FileName: "raw.mov", MimeType: "video/quicktime",
		FileData: make([]byte, federation.DefaultMaxAttachmentBytes+1)}

	_, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"bob@example.test"}, "Video", "<p>x</p>",
		[]NewAttachment{raw})
	var tooLarge *TooLargeError
	if !errors.As(err, &tooLarge) || err.Error() != "raw.mov is larger than the 25 MB limit" {
		t.Fatalf("AddThread = %v, want raw.mov refused as larger than 25 MB", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM "g_message"`); n != 0 {
		t.Errorf("%d messages were written", n)
	}
}
