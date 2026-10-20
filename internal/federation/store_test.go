package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mevdschee/webmail-server/internal/store"
	"github.com/mevdschee/webmail-server/internal/store/storetest"
)

// These tests run the federation queries against a real database: SQLite by
// default, PostgreSQL when WEBMAIL_TEST_POSTGRES_DSN is set (ADR 19).

func newStoreService(t *testing.T, db *store.DB) *Service {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Domains: []LocalDomain{{Domain: "example.test", key: key}}}
	s, err := New(db, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func enqueue(t *testing.T, s *Service, peer string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		err := s.db.Tx(ctx, func(tx *sql.Tx) error {
			return s.Enqueue(ctx, tx, Outbound{
				PeerDomain: peer, OriginDomain: "example.test",
				ThreadFedID: "0192f0c0-0000-7000-8000-000000000001", Subject: "s",
				MessageFedID: fmt.Sprintf("0192f0c0-0000-7000-8000-%012d", i+2),
				SenderEmail:  "alice@example.test", SenderName: "Alice", Body: "héllo",
				SentAt: s.db.Now(), Recipients: []string{"bob@" + peer},
			})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestOutboxLeases(t *testing.T) {
	db := storetest.Open(t)
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	db.Clock = func() time.Time { return now }
	ctx := context.Background()
	first := newStoreService(t, db)
	second := newStoreService(t, db)
	enqueue(t, first, "peer.test", 3)

	peers, err := first.duePeers(ctx)
	if err != nil || len(peers) != 1 || peers[0] != "peer.test" {
		t.Fatalf("duePeers = %v, %v", peers, err)
	}
	claimed, err := first.claim(ctx, "peer.test", 2)
	if err != nil || len(claimed) != 2 || claimed[0].ID > claimed[1].ID {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if string(claimed[0].Body) == "" || claimed[0].Attempts != 0 || !claimed[0].CreatedAt.Equal(now) {
		t.Errorf("claimed row = %+v", claimed[0])
	}
	rest, err := second.claim(ctx, "peer.test", 10)
	if err != nil || len(rest) != 1 {
		t.Fatalf("the second worker claimed %d rows, want the one left: %v", len(rest), err)
	}
	if more, _ := second.claim(ctx, "peer.test", 10); len(more) != 0 {
		t.Errorf("leased rows were claimed again: %+v", more)
	}

	// A transient failure releases the row until its retry time.
	first.reschedule(ctx, claimed[0], 503, "unavailable", time.Minute)
	if due, _ := second.claim(ctx, "peer.test", 10); len(due) != 0 {
		t.Errorf("a rescheduled row was due early: %+v", due)
	}
	first.finish(ctx, claimed[1], "delivered", 202, "")

	// A worker that died holding a lease loses it when the lease runs out.
	now = now.Add(Lease + time.Minute)
	again, err := second.claim(ctx, "peer.test", 10)
	if err != nil || len(again) != 2 {
		t.Fatalf("after the leases expired %d rows were claimable, want 2: %v", len(again), err)
	}
	if again[0].Attempts != 1 {
		t.Errorf("the rescheduled row has %d attempts", again[0].Attempts)
	}

	// Delivered rows are swept once the dedup retention has passed.
	now = now.Add(DedupRetention + time.Hour)
	if err := first.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.SelectInt(ctx, db, `SELECT COUNT(*) FROM "federation_outbox"`); n != 2 {
		t.Errorf("%d outbox rows after the sweep, want the two undelivered", n)
	}
}

// Workers racing for the same rows never both get one. On PostgreSQL this is
// the re-check after a lock wait; on SQLite it is the single writer.
func TestConcurrentClaimsAreDisjoint(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	workers := []*Service{newStoreService(t, db), newStoreService(t, db), newStoreService(t, db), newStoreService(t, db)}
	enqueue(t, workers[0], "peer.test", 40)

	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w *Service) {
			defer wg.Done()
			for {
				rows, err := w.claim(ctx, "peer.test", 3)
				if err != nil {
					t.Error(err)
					return
				}
				if len(rows) == 0 {
					return
				}
				mu.Lock()
				for _, r := range rows {
					seen[r.ID]++
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if len(seen) != 40 {
		t.Errorf("%d rows claimed, want 40", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %d claimed %d times", id, n)
		}
	}
}

func TestPeerCache(t *testing.T) {
	db := storetest.Open(t)
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	db.Clock = func() time.Time { return now }
	ctx := context.Background()
	s := newStoreService(t, db)

	if err := s.RegisterLocalDomains(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterLocalDomains(ctx); err != nil {
		t.Fatalf("registering twice: %v", err)
	}
	if d, _, _, err := s.cachedPeer(ctx, "example.test"); err != nil || d != nil {
		t.Errorf("a local domain was read as a peer: %+v, %v", d, err)
	}

	s.recordDiscoveryFailure(ctx, "peer.test")
	d, _, failedAt, err := s.cachedPeer(ctx, "peer.test")
	if err != nil || d != nil || !failedAt.Equal(now) {
		t.Fatalf("after a failure: %+v, %v, %v", d, failedAt, err)
	}
	if err := s.storePeer(ctx, &Descriptor{Domain: "peer.test", PublicKey: "k1",
		PreviousPublicKeys: []string{"k0"}, Versions: []string{Version}, Registrar: true}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := s.storePeer(ctx, &Descriptor{Domain: "peer.test", PublicKey: "k2", Versions: []string{Version}}); err != nil {
		t.Fatal(err)
	}
	d, discoveredAt, failedAt, err := s.cachedPeer(ctx, "peer.test")
	if err != nil || d == nil || d.PublicKey != "k2" || d.Registrar || !discoveredAt.Equal(now) || !failedAt.IsZero() {
		t.Fatalf("cached peer = %+v, %v, %v, %v", d, discoveredAt, failedAt, err)
	}
	if n, _ := store.SelectInt(ctx, db, `SELECT COUNT(*) FROM "domain"`); n != 2 {
		t.Errorf("%d domain rows, want one local and one peer", n)
	}
}

func TestReplayAndDedup(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	s := newStoreService(t, db)
	if r := s.claimNonce(ctx, "peer.test", "inbound", "n1"); r != nil {
		t.Fatalf("first nonce refused: %+v", r)
	}
	if r := s.claimNonce(ctx, "peer.test", "inbound", "n1"); r == nil || r.Code != ErrReplay {
		t.Errorf("a replayed nonce was accepted: %+v", r)
	}
	if r := s.claimNonce(ctx, "other.test", "inbound", "n1"); r != nil {
		t.Errorf("another sender's nonce was refused: %+v", r)
	}
	if seen, err := s.alreadyAccepted(ctx, "peer.test", "e1"); err != nil || seen {
		t.Fatalf("alreadyAccepted = %v, %v", seen, err)
	}
	for i := 0; i < 2; i++ {
		if err := s.recordAccepted(ctx, "peer.test", "e1"); err != nil {
			t.Fatal(err)
		}
	}
	if seen, err := s.alreadyAccepted(ctx, "peer.test", "e1"); err != nil || !seen {
		t.Fatalf("alreadyAccepted = %v, %v", seen, err)
	}
}

// The send time is taken as given, past or future: it is what orders the
// thread on every Domain, so no Domain may replace it (RFC 5.5).
func TestSendTimesAreTakenAsGiven(t *testing.T) {
	db := storetest.Open(t)
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	db.Clock = func() time.Time { return now }
	s := newStoreService(t, db)
	for _, sent := range []time.Time{now.Add(ClockSkew + time.Hour), now.Add(-48 * time.Hour)} {
		e := Envelope{
			Thread:     Thread{FederationID: "0192f0c0-0000-7000-8000-000000000001"},
			Message:    Message{FederationID: "0192f0c0-0000-7000-8000-000000000002", SenderEmail: "pete@peer.test", SentAt: FormatTimestamp(sent)},
			Recipients: []string{"bob@example.test"},
		}
		in, r := s.toInbound(&e)
		if r != nil {
			t.Fatalf("toInbound: %v", r)
		}
		if !in.SentAt.Equal(sent) {
			t.Errorf("sent %s became %s", sent, in.SentAt)
		}
	}
}

func TestOutboxListsWhereEachEnvelopeWent(t *testing.T) {
	db := storetest.Open(t)
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	db.Clock = func() time.Time { return now }
	ctx := context.Background()
	s := newStoreService(t, db)
	enqueue(t, s, "peer.test", 1)
	enqueue(t, s, "other.test", 1)
	claimed, err := s.claim(ctx, "peer.test", 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	s.finish(ctx, claimed[0], "delivered", 202, "")

	entries, err := s.Outbox(ctx)
	if err != nil || len(entries) != 2 {
		t.Fatalf("Outbox = %+v, %v", entries, err)
	}
	delivered, pending := entries[0], entries[1]
	if delivered.PeerDomain != "peer.test" || delivered.Kind != "envelope" ||
		delivered.Status != "delivered" || delivered.DeliveredAt == nil ||
		len(delivered.Recipients) != 1 || delivered.Recipients[0] != "bob@peer.test" {
		t.Errorf("delivered entry = %+v", delivered)
	}
	if pending.PeerDomain != "other.test" || pending.Status != "pending" || pending.DeliveredAt != nil ||
		!pending.CreatedAt.Equal(now) {
		t.Errorf("pending entry = %+v", pending)
	}
}

// recordingDeliverer keeps the bounces the outbox asks for.
type recordingDeliverer struct{ bounces []BounceNotice }

func (d *recordingDeliverer) DeliverInbound(ctx context.Context, in Inbound) (int, []string, error) {
	return 0, nil, nil
}
func (d *recordingDeliverer) Undelivered(ctx context.Context, in Inbound) ([]string, error) {
	return nil, nil
}
func (d *recordingDeliverer) Bounce(ctx context.Context, b BounceNotice) error {
	d.bounces = append(d.bounces, b)
	return nil
}

// A bounce says when its recipients do not exist, so the sender's Domain stops
// sending to them; a Domain that was down says nothing of the kind (RFC 6.5).
func TestBouncesSayWhenRecipientsAreUnknown(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	s := newStoreService(t, db)
	d := &recordingDeliverer{}
	s.Attach(d)
	enqueue(t, s, "peer.test", 3)
	rows, err := s.claim(ctx, "peer.test", 10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("claim = %d, %v", len(rows), err)
	}

	s.bounceRefused(ctx, rows[0], []Refusal{{Address: "bob@peer.test", Error: ErrUnknownRecipient}})
	s.fail(ctx, rows[1], 404, "no_recipients")
	s.fail(ctx, rows[2], 503, "timed_out")

	if len(d.bounces) != 3 {
		t.Fatalf("%d bounces, want 3", len(d.bounces))
	}
	for i, want := range []bool{true, true, false} {
		b := d.bounces[i]
		if b.Refused != want {
			t.Errorf("bounce %d (%d %s): Refused = %v, want %v", i, b.HTTPStatus, b.ErrorCode, b.Refused, want)
		}
		if b.ThreadFedID != "0192f0c0-0000-7000-8000-000000000001" {
			t.Errorf("bounce %d names thread %q", i, b.ThreadFedID)
		}
	}
}
