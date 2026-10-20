package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strconv"
	"strings"
	"time"
)

// sqliteTime is how a time is written to SQLite. It is the format SQLite's own
// CURRENT_TIMESTAMP produces, so column defaults and values passed in from Go
// compare and sort as text the way they would as times.
const sqliteTime = "2006-01-02 15:04:05"

// connector opens connections through the engine's driver and wraps each one,
// so the domain packages can hand plain *sql.DB and *sql.Tx values around and
// still speak one dialect. It is the only place the engines differ in how a
// statement is sent.
type connector struct {
	open   func(ctx context.Context) (driver.Conn, error)
	driver driver.Driver
	engine Engine
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	base, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	return &conn{Conn: base, engine: c.engine}, nil
}

func (c *connector) Driver() driver.Driver { return c.driver }

// conn rewrites every statement before the engine's driver sees it:
// placeholders are rebound for PostgreSQL, and times are normalised to UTC
// whole seconds (written as text for SQLite, ADR 19).
type conn struct {
	driver.Conn
	engine Engine
}

func (c *conn) rebind(query string) string {
	if c.engine == Postgres {
		return Rebind(query)
	}
	return query
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.Conn.Prepare(c.rebind(query))
}

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, c.rebind(query))
	}
	return c.Conn.Prepare(c.rebind(query))
}

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin() //nolint:staticcheck // fallback for drivers without BeginTx
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return e.ExecContext(ctx, c.rebind(query), args)
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return q.QueryContext(ctx, c.rebind(query), args)
}

func (c *conn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *conn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *conn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// CheckNamedValue normalises times before the engine's driver converts them.
// Times are stored in UTC (ADR 8) at whole seconds, which is what MySQL kept
// and what makes SQLite's text comparison agree with time order.
func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	var t time.Time
	switch v := nv.Value.(type) {
	case time.Time:
		t = v
	case *time.Time:
		if v == nil {
			nv.Value = nil
			return nil
		}
		t = *v
	case sql.NullTime:
		if !v.Valid {
			nv.Value = nil
			return nil
		}
		t = v.Time
	default:
		if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
			return checker.CheckNamedValue(nv)
		}
		return driver.ErrSkip
	}
	t = t.UTC().Truncate(time.Second)
	if c.engine == SQLite {
		nv.Value = t.Format(sqliteTime)
	} else {
		nv.Value = t
	}
	return nil
}

// Rebind turns the ? placeholders the code is written with into PostgreSQL's
// $1..$n. Question marks inside string literals, quoted identifiers and
// comments are left alone.
func Rebind(query string) string {
	if !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		ch := query[i]
		switch ch {
		case '\'', '"':
			end := closing(query, i, ch)
			b.WriteString(query[i:end])
			i = end - 1
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				end := strings.IndexByte(query[i:], '\n')
				if end < 0 {
					end = len(query) - i
				}
				b.WriteString(query[i : i+end])
				i += end - 1
				continue
			}
			b.WriteByte(ch)
		case '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// closing returns the index just past the quote that closes the one at start,
// treating a doubled quote as an escaped one.
func closing(query string, start int, quote byte) int {
	for i := start + 1; i < len(query); i++ {
		if query[i] != quote {
			continue
		}
		if i+1 < len(query) && query[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(query)
}
