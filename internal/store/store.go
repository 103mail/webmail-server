// Package store owns the database connection and the small query helpers the
// domain packages share. It holds no domain knowledge of its own.
//
// Production runs PostgreSQL and every test environment runs SQLite (ADR 19).
// The domain packages write one portable dialect: ANSI quoted identifiers, ?
// placeholders, the current time passed in from Go, ON CONFLICT and RETURNING.
// What the engines do not share is confined to this package: opening the
// connection, rebinding placeholders, engine settings, and full-text search.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"modernc.org/sqlite"
)

// Engine is the database engine behind a DB.
type Engine string

const (
	Postgres Engine = "postgres"
	SQLite   Engine = "sqlite"
)

// DB wraps *sql.DB with the helpers the domain packages need.
type DB struct {
	*sql.DB
	Engine Engine
	// Clock is the time source for every timestamp the application writes.
	// Queries never read the engine's clock, so a test can move time by
	// replacing this.
	Clock func() time.Time
}

// Queryer is satisfied by both *sql.DB and *sql.Tx, so domain helpers can run
// inside or outside a transaction.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Open connects to the database a DSN names. The scheme selects the engine:
// postgres:// (or postgresql://) for PostgreSQL, sqlite:// for an SQLite file,
// as in sqlite:///var/lib/webmail/webmail.db.
func Open(dsn string) (*DB, error) {
	scheme, rest, _ := strings.Cut(dsn, "://")
	switch strings.ToLower(scheme) {
	case "postgres", "postgresql":
		return openPostgres(dsn)
	case "sqlite":
		return openSQLite(rest)
	}
	return nil, fmt.Errorf("unsupported database DSN %q: want postgres:// or sqlite://", redact(dsn))
}

func openPostgres(dsn string) (*DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	// Columns are timestamp without time zone holding UTC (ADR 8). The session
	// zone decides what CURRENT_TIMESTAMP defaults write, so it has to be UTC
	// too, whatever the server is configured with.
	cfg.RuntimeParams["timezone"] = "UTC"
	base := stdlib.GetConnector(*cfg)
	db := sql.OpenDB(&connector{open: base.Connect, driver: base.Driver(), engine: Postgres})
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)
	return newDB(db, Postgres), nil
}

// openSQLite opens a file in WAL mode with a busy timeout, so a Domain's API
// and worker can share it, and with foreign keys enforced, so tests see the
// same references and cascades as PostgreSQL. Write transactions take the
// write lock when they begin rather than at their first write, which is what
// lets two processes wait on each other instead of failing with SQLITE_BUSY.
func openSQLite(path string) (*DB, error) {
	path, query, _ := strings.Cut(path, "?")
	if path == "" {
		return nil, fmt.Errorf("sqlite DSN names no file")
	}
	params, err := url.ParseQuery(query)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	params.Add("_pragma", "foreign_keys(1)")
	params.Add("_pragma", "busy_timeout(10000)")
	params.Add("_pragma", "journal_mode(WAL)")
	params.Add("_pragma", "synchronous(NORMAL)")
	params.Set("_txlock", "immediate")
	name := "file:" + path + "?" + params.Encode()

	drv := &sqlite.Driver{}
	open := func(context.Context) (driver.Conn, error) { return drv.Open(name) }
	db := sql.OpenDB(&connector{open: open, driver: drv, engine: SQLite})
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	return newDB(db, SQLite), nil
}

func newDB(db *sql.DB, engine Engine) *DB {
	return &DB{DB: db, Engine: engine, Clock: time.Now}
}

// redact drops the password from a DSN so it can appear in an error.
func redact(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn
	}
	return u.Redacted()
}

// OffsetClock is a clock that runs ahead of the real one by the duration in
// the file at path, such as "90s". A test stack sets it through
// WEBMAIL_CLOCK_FILE so the end-to-end suite can let time pass without
// waiting: it moves the offset forward and every Domain's retries, budgets
// and probes see the later time at once. A missing or unreadable file means
// no offset. Production never sets it.
func OffsetClock(path string) func() time.Time {
	return OffsetClockFrom(path, time.Now)
}

// OffsetClockFrom is OffsetClock over a given real clock, for tests. The file
// is read on every call, so a change is seen at once.
func OffsetClockFrom(path string, real func() time.Time) func() time.Time {
	return func() time.Time {
		raw, err := os.ReadFile(path)
		if err != nil {
			return real()
		}
		offset, err := time.ParseDuration(strings.TrimSpace(string(raw)))
		if err != nil {
			return real()
		}
		return real().Add(offset)
	}
}

// Now is the current time as the application stores it: UTC, whole seconds.
func (db *DB) Now() time.Time {
	return db.Clock().UTC().Truncate(time.Second)
}

// Tx runs fn inside a transaction, rolling back on error or panic.
func (db *DB) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Placeholders renders "?,?,?" for an IN clause of n values. It returns "NULL"
// for n == 0 so the query stays valid and matches nothing.
func Placeholders(n int) string {
	if n == 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Args converts a slice of ids into the []any that IN clauses need.
func Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// SelectInt returns the first column of the first row, or 0 when there is no
// row or the value is NULL. This mirrors how the PHP code treats a missing id.
func SelectInt(ctx context.Context, q Queryer, query string, args ...any) (int64, error) {
	var v sql.NullInt64
	err := q.QueryRowContext(ctx, query, args...).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return v.Int64, nil
}

// SelectString returns the first column of the first row, or "" when there is
// no row or the value is NULL.
func SelectString(ctx context.Context, q Queryer, query string, args ...any) (string, error) {
	var v sql.NullString
	err := q.QueryRowContext(ctx, query, args...).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v.String, nil
}

// SelectStrings returns the first column of every row.
func SelectStrings(ctx context.Context, q Queryer, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v.String)
	}
	return out, rows.Err()
}

// SelectInts returns the first column of every row, as integers.
func SelectInts(ctx context.Context, q Queryer, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v sql.NullInt64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v.Int64)
	}
	return out, rows.Err()
}

// SelectCounts returns a map of the first column to the second, for the
// "SELECT key, COUNT(...) ... GROUP BY key" queries the listings use.
func SelectCounts(ctx context.Context, q Queryer, query string, args ...any) (map[int64]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var k, v sql.NullInt64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k.Int64] = v.Int64
	}
	return out, rows.Err()
}

// SelectPairs returns a map of the first column to the second for string keys.
func SelectPairs(ctx context.Context, q Queryer, query string, args ...any) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k.String] = v.String
	}
	return out, rows.Err()
}

// Insert runs an INSERT and returns the generated id. The statement is sent
// with RETURNING "id" appended, because PostgreSQL has no LastInsertId.
func Insert(ctx context.Context, q Queryer, query string, args ...any) (int64, error) {
	var id int64
	if err := q.QueryRowContext(ctx, query+` RETURNING "id"`, args...).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// Affected runs a statement and returns the number of rows it changed.
func Affected(ctx context.Context, q Queryer, query string, args ...any) (int64, error) {
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// NullTime scans a timestamp from either engine. PostgreSQL always returns a
// time, and SQLite does for a timestamp column, but SQLite returns the stored
// text for an expression such as MAX("created_at"), which loses the column's
// declared type. sql.NullTime cannot read that.
type NullTime struct {
	Time  time.Time
	Valid bool
}

// Scan implements sql.Scanner.
func (t *NullTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		t.Time, t.Valid = time.Time{}, false
		return nil
	case time.Time:
		t.Time, t.Valid = v.UTC(), true
		return nil
	case string:
		return t.parse(v)
	case []byte:
		return t.parse(string(v))
	}
	return fmt.Errorf("cannot scan %T into a time", src)
}

func (t *NullTime) parse(s string) error {
	for _, layout := range []string{sqliteTime, "2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02T15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.Time, t.Valid = parsed.UTC(), true
			return nil
		}
	}
	return fmt.Errorf("cannot parse %q as a time", s)
}

// Address normalises an email address for storage and comparison. Addresses
// are kept lowercase because PostgreSQL and SQLite compare text exactly, where
// MySQL's default collation ignored case (ADR 19). Every address that reaches
// a query from outside goes through this first.
func Address(address string) string {
	return strings.ToLower(strings.TrimSpace(address))
}
