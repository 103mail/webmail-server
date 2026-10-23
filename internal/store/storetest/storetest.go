// Package storetest opens a fresh database with the full schema for a test.
//
// Tests run on SQLite, as every test environment does (ADR 19). Setting
// WEBMAIL_TEST_POSTGRES_DSN to a PostgreSQL server runs the same tests there
// instead, each in a schema of its own that is dropped afterwards. That is the
// way to check the portable dialect against the production engine.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/103mail/webmail-server/internal/store"
)

// Open returns an empty database with the schema created, closed when the test
// ends.
func Open(t testing.TB) *store.DB {
	t.Helper()
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "webmail.db")
	if admin := os.Getenv("WEBMAIL_TEST_POSTGRES_DSN"); admin != "" {
		dsn = postgresSchema(t, admin)
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.CreateSchema(context.Background()); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	return db
}

// postgresSchema creates a schema for one test and returns a DSN that uses it.
func postgresSchema(t testing.TB, admin string) string {
	t.Helper()
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	name := "test_" + hex.EncodeToString(buf)
	db, err := store.Open(admin)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+name+`"`); err != nil {
		db.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DROP SCHEMA "`+name+`" CASCADE`)
		db.Close()
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse WEBMAIL_TEST_POSTGRES_DSN: %v", err)
	}
	q := u.Query()
	q.Set("search_path", name)
	u.RawQuery = q.Encode()
	return u.String()
}
