// Package store owns the database connection and the small query helpers the
// domain packages share. It holds no domain knowledge of its own.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// DB wraps *sql.DB with the helpers the domain packages need.
type DB struct {
	*sql.DB
}

// Queryer is satisfied by both *sql.DB and *sql.Tx, so domain helpers can run
// inside or outside a transaction.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Open connects to MySQL. Times are read as UTC so the API can format them as
// RFC 3339 without guessing a server timezone.
func Open(dsn string) (*DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+00:00'"

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &DB{db}, nil
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

// Insert runs an INSERT and returns the generated id.
func Insert(ctx context.Context, q Queryer, query string, args ...any) (int64, error) {
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Affected runs a statement and returns the number of rows it changed.
func Affected(ctx context.Context, q Queryer, query string, args ...any) (int64, error) {
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
