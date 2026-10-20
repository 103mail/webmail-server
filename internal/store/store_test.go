package store_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/mevdschee/webmail-server/internal/store"
	"github.com/mevdschee/webmail-server/internal/store/storetest"
)

func TestPlaceholders(t *testing.T) {
	tests := map[int]string{0: "NULL", 1: "?", 3: "?,?,?"}
	for n, want := range tests {
		if got := store.Placeholders(n); got != want {
			t.Errorf("Placeholders(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestArgs(t *testing.T) {
	if got := store.Args([]int64{1, 2}); !reflect.DeepEqual(got, []any{int64(1), int64(2)}) {
		t.Errorf("Args = %v", got)
	}
	if got := store.Args(nil); len(got) != 0 {
		t.Errorf("Args(nil) = %v, want empty", got)
	}
}

func TestRebind(t *testing.T) {
	tests := map[string]string{
		`SELECT "id" FROM "user" WHERE "a" = ? AND "b" IN (?,?)`: `SELECT "id" FROM "user" WHERE "a" = $1 AND "b" IN ($2,$3)`,
		`SELECT 'what?' FROM "x" WHERE "y" = ?`:                  `SELECT 'what?' FROM "x" WHERE "y" = $1`,
		`SELECT 'it''s?', "odd?""name" FROM "x" WHERE "y" = ?`:   `SELECT 'it''s?', "odd?""name" FROM "x" WHERE "y" = $1`,
		"SELECT 1 -- why?\nWHERE \"a\" = ?":                      "SELECT 1 -- why?\nWHERE \"a\" = $1",
		`SELECT 1`:                                               `SELECT 1`,
	}
	for in, want := range tests {
		if got := store.Rebind(in); got != want {
			t.Errorf("Rebind(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestOpenRejectsUnknownScheme(t *testing.T) {
	for _, dsn := range []string{"webmail:webmail@tcp(127.0.0.1:3306)/webmail", "mysql://x", "sqlite://"} {
		if db, err := store.Open(dsn); err == nil {
			db.Close()
			t.Errorf("Open(%q) succeeded, want an error", dsn)
		}
	}
}

func TestCreateSchemaIsIdempotent(t *testing.T) {
	db := storetest.Open(t)
	created, err := db.CreateSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("CreateSchema created a schema in a database that has one")
	}
}

func TestInsertReturnsTheID(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	first, err := store.Insert(ctx, db, `INSERT INTO "customer" ("name") VALUES (?)`, "a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Insert(ctx, db, `INSERT INTO "customer" ("name") VALUES (?)`, "b")
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 || second != first+1 {
		t.Errorf("ids %d and %d, want consecutive non-zero ids", first, second)
	}
}

// Times go in as UTC whole seconds whatever their zone, and come back the same
// on both engines, including through an aggregate, which SQLite returns as text.
func TestTimesRoundTrip(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("no zoneinfo:", err)
	}
	in := time.Date(2026, 3, 29, 3, 30, 15, 987654321, amsterdam)
	want := time.Date(2026, 3, 29, 1, 30, 15, 0, time.UTC)

	id, err := store.Insert(ctx, db,
		`INSERT INTO "g_thread" ("subject_line", "created_at", "updated_at") VALUES (?, ?, ?)`,
		"times", in, &in)
	if err != nil {
		t.Fatal(err)
	}
	var got time.Time
	if err := db.QueryRowContext(ctx, `SELECT "created_at" FROM "g_thread" WHERE "id" = ?`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Errorf("created_at = %v, want %v", got, want)
	}
	var max store.NullTime
	if err := db.QueryRowContext(ctx, `SELECT MAX("updated_at") FROM "g_thread"`).Scan(&max); err != nil {
		t.Fatal(err)
	}
	if !max.Valid || !max.Time.Equal(want) {
		t.Errorf("MAX(updated_at) = %v, want %v", max, want)
	}

	// A time written by Go compares correctly with one the engine defaulted.
	n, err := store.SelectInt(ctx, db,
		`SELECT COUNT(*) FROM "g_thread" WHERE "id" = ? AND "created_at" < ?`, id, db.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("a stored time does not compare as earlier than now")
	}
}

func TestSelectInts(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	for _, name := range []string{"a", "b"} {
		if _, err := store.Insert(ctx, db, `INSERT INTO "customer" ("name") VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := store.SelectInts(ctx, db, `SELECT "id" FROM "customer" ORDER BY "id"`)
	if err != nil || len(ids) != 2 || ids[0] >= ids[1] {
		t.Errorf("SelectInts = %v, %v", ids, err)
	}
	if none, err := store.SelectInts(ctx, db, `SELECT "id" FROM "customer" WHERE "id" < 0`); err != nil || len(none) != 0 {
		t.Errorf("SelectInts with no rows = %v, %v", none, err)
	}
}

func TestNowIsUTCWholeSeconds(t *testing.T) {
	db := storetest.Open(t)
	db.Clock = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 600, time.FixedZone("x", 3600)) }
	if got, want := db.Now(), time.Date(2026, 1, 2, 2, 4, 5, 0, time.UTC); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("Now() = %v, want %v", got, want)
	}
}

func TestForeignKeysCascade(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	thread, err := store.Insert(ctx, db, `INSERT INTO "g_thread" ("subject_line") VALUES (?)`, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO "g_subscription" ("g_thread_id", "email") VALUES (?, ?)`,
		thread+1000, "x@example.com"); err == nil {
		t.Error("a subscription to a missing thread was accepted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO "g_subscription" ("g_thread_id", "email") VALUES (?, ?)`,
		thread, "x@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM "g_thread" WHERE "id" = ?`, thread); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.SelectInt(ctx, db, `SELECT COUNT(*) FROM "g_subscription"`); n != 0 {
		t.Errorf("%d subscriptions survived their thread", n)
	}
}

func TestTxRollsBack(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	failed := errors.New("fail")
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO "customer" ("name") VALUES (?)`, "gone"); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("Tx = %v, want the callback's error", err)
	}
	if n, _ := store.SelectInt(ctx, db, `SELECT COUNT(*) FROM "customer"`); n != 0 {
		t.Errorf("%d customers after a rolled back insert", n)
	}
}
