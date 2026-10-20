# 19. PostgreSQL in production, SQLite under test

Status: accepted
Date: 2026-10-19

## Context

The PHP monolith runs on MySQL. Production should run on PostgreSQL: it has the
guarantees we lean on (transactional DDL for migrations over immutable history,
`SKIP LOCKED`, `RETURNING`, real booleans and time zones), and it is what we
want to operate. The end-to-end suite in `../webmail-e2e` starts four or five
Domains per run, each with its own database, and a database server per Domain
makes that stack slow to start and heavy to run in parallel. An SQLite file per
Domain costs nothing to create and nothing to throw away.

## Decision

**Production runs PostgreSQL. Every test environment runs SQLite**, including
the end-to-end stack, CI and local development. The Go backend does not run on
MySQL.

The application speaks **one portable SQL dialect** that both engines accept, so
the queries under test are the queries in production. The differences are
confined to `internal/store`: opening the connection, rebinding placeholders,
engine settings, and full-text search.

Drivers are `github.com/jackc/pgx/v5/stdlib` and `modernc.org/sqlite`, both pure
Go, so builds stay cgo-free. `WEBMAIL_DSN` selects the engine by scheme
(`postgres://` or `sqlite://`).

### The portable dialect

| MySQL | Portable form | Why |
| --- | --- | --- |
| `` `name` `` | `"name"` | ANSI quoting; `user` is reserved in PostgreSQL |
| `?` placeholders | `?` in source, rebound to `$1..$n` for PostgreSQL | One spelling in the code |
| `NOW()` | the current time passed in from Go, in UTC | No engine clock in queries, so tests can move the clock |
| `INSERT IGNORE` | `INSERT ... ON CONFLICT DO NOTHING` | Skips only rows that violate a unique constraint |
| `ON DUPLICATE KEY UPDATE x = VALUES(x)` | `ON CONFLICT (cols) DO UPDATE SET x = excluded.x` | Supported by both |
| `LastInsertId()` | `INSERT ... RETURNING id` | pgx has no `LastInsertId` |
| `FOR UPDATE SKIP LOCKED` claim | lease: `UPDATE ... SET claimed_by = ?, claimed_until = ? WHERE id IN (SELECT ...) AND (claimed_until IS NULL OR claimed_until < ?) RETURNING ...` | PostgreSQL re-checks the outer condition after a lock wait, so two workers cannot claim one row; SQLite has a single writer |

The schema is the DBML, rendered per engine by `dbml-tools tosql` (0.9.7 or
later for SQLite). It uses only types both engines accept: `bigint`, `integer`,
`boolean`, `timestamp`, `text` and `varchar`.

### Full-text search is engine-specific

No full-text syntax is shared by the two engines, and `LIKE` would change what
users get, so each keeps a real index:

| | PostgreSQL | SQLite |
| --- | --- | --- |
| Index | stored generated `tsvector` column with a GIN index | FTS5 external-content table kept current by triggers |
| Tokenising | `simple`: lowercase, no stemming, no stopwords | `unicode61` with `remove_diacritics 0` |
| Query | `tsquery` built in Go | FTS5 `MATCH` expression built in Go |

The searched columns are `g_thread.subject_line`, `g_message.body`,
`g_message.sender_name`, `contact.name`, `contact.email`,
`g_chat_message.body` and `g_chat_message.sender_name`.

The search string is parsed once, in Go, keeping MySQL's boolean-mode syntax
(`+word`, `-word`, `"a phrase"`, `word*`), and rendered for either engine. Raw
input never reaches an engine's query parser. Both tokenisers lowercase without
stemming, so the engines match the same words. Search sits behind one interface
in `internal/store` that returns a `WHERE` condition and its arguments.

The search indexes, which DBML cannot describe, live in
`schema/search.postgresql.sql` and `schema/search.sqlite.sql` and are applied
after the tables.

### Rules the dialect cannot express

- **Email addresses are stored and compared lowercase**, since PostgreSQL and
  SQLite compare exactly where MySQL's default collation does not.
- **Times are UTC** (ADR 8). SQLite stores them as ISO 8601 text.
- **SQLite runs in WAL mode with a busy timeout**, so a Domain's API and worker
  can share one file.
- **SQLite connections set `PRAGMA foreign_keys = ON`**, so tests enforce the
  same references and cascades as PostgreSQL.

## Consequences

- Each Domain in the end-to-end stack is an API, a worker and an SQLite file, so
  stories can create Domains cheaply and run in parallel.
- The cutover is also an engine move. Parity is checked by migrating a copy of
  production to PostgreSQL (pgloader for the data, the DBML for the schema) and
  comparing the PHP backend on the original with the Go backend on the copy:
  row counts and checksums per table first, then endpoint by endpoint.
- After the switch, writes live only in PostgreSQL. Rolling back to the PHP
  backend loses mail received after the switch.
- Tests cannot prove PostgreSQL's locking, planner and type behaviour, and
  search runs different SQL per engine. Running the end-to-end suite against
  PostgreSQL before each release is the recommended guard; it is not decided
  here. Search has unit tests that render one parsed query for both engines.
- Writes to searched tables also maintain the search index, which is one extra
  index write per message.
