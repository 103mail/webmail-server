package email

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mevdschee/webmail-server/internal/blob"
	"github.com/mevdschee/webmail-server/internal/federation"
	"github.com/mevdschee/webmail-server/internal/store"
	"github.com/mevdschee/webmail-server/internal/store/storetest"
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
		mailboxes, err := f.svc.ListMailboxes(f.ctx, alice, tc.search)
		if err != nil {
			t.Fatalf("search %q in mailboxes: %v", tc.search, err)
		}
		if got := mailboxes[0].UnreadCount; got != int64(tc.threads) {
			t.Errorf("search %q counts %d threads in the mailbox, want %d", tc.search, got, tc.threads)
		}
		folders, err := f.svc.ListFolders(f.ctx, alice, aliceMailbox, tc.search)
		if err != nil {
			t.Fatalf("search %q in folders: %v", tc.search, err)
		}
		if got := folders[0].UnreadCount; got != int64(tc.threads) {
			t.Errorf("search %q counts %d threads in the folder, want %d", tc.search, got, tc.threads)
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
	if threads, _ := f.svc.ListThreads(f.ctx, alice, aliceInbox, ""); len(threads) != 1 {
		t.Error("deleting Bob's copy removed Alice's")
	}
}

// A recipient with no account is onboarded with a customer of their own and a
// mailbox that belongs to that customer.
func TestOnboarding(t *testing.T) {
	f := newFixture(t)
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")
	// A customer with no user, so customer and user ids no longer line up.
	if _, err := store.Insert(f.ctx, f.db, `INSERT INTO "customer" ("name") VALUES (?)`, "spare"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"dave@example.test"}, "Hello", "<p>Hi</p>", nil); err != nil {
		t.Fatal(err)
	}
	var customerID, userCustomerID int64
	if err := f.db.QueryRowContext(f.ctx,
		`SELECT "alias"."customer_id", "user"."customer_id" FROM "alias" `+
			`JOIN "user" ON "user"."username" = "alias"."email" WHERE "alias"."email" = ?`,
		"dave@example.test").Scan(&customerID, &userCustomerID); err != nil {
		t.Fatal(err)
	}
	if customerID != userCustomerID {
		t.Errorf("Dave's mailbox belongs to customer %d, his user to %d", customerID, userCustomerID)
	}
	if n := f.count(`SELECT COUNT(*) FROM "thread" WHERE "customer_id" = ?`, customerID); n != 1 {
		t.Errorf("Dave has %d threads", n)
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

	delivered, err := f.svc.DeliverInbound(f.ctx, federation.Inbound{
		OriginDomain: "peer.test", ThreadFedID: "0192f0c0-0000-7000-8000-000000000001",
		Subject: "Federated", MessageFedID: "0192f0c0-0000-7000-8000-000000000002",
		SenderEmail: "Pete@peer.test", SenderName: "Pete", Body: "From a peer",
		SentAt:     f.now.Add(-time.Minute),
		Recipients: []string{"bob@example.test", "pete@peer.test"},
		Attachments: []federation.InboundAttachment{{FederationID: "0192f0c0-0000-7000-8000-000000000003",
			FileName: "a/b.txt", MimeType: "text/plain", Data: []byte("x")}},
	})
	// With federation off every address is local, so Pete is onboarded too.
	if err != nil || delivered != 2 {
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
func TestRemoteRecipientsAreQueued(t *testing.T) {
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
	alice, _, aliceInbox := f.account("alice@example.test", "Alice")

	if _, err := f.svc.AddThread(f.ctx, alice, aliceInbox, []string{"Bob@Peer.test", "carol@peer.test"},
		"Across", "<p>Hi</p>", []NewAttachment{{FileName: "a.txt", MimeType: "text/plain", FileData: []byte("a")}}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT COUNT(*) FROM "federation_outbox" WHERE "peer_domain" = 'peer.test' AND "status" = 'pending'`); n != 1 {
		t.Errorf("%d envelopes queued for peer.test, want one for both recipients", n)
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
