# 19. PostgreSQL in production, SQLite under test

Status: proposed
Date: 2026-10-19

Supersedes the MySQL choice in ADR 12 and the shared-database cutover in ADR 14.

## Context

The server runs on MySQL because the PHP monolith does, and ADR 14 checked the
port by running both backends against the same database. That made the port
verifiable, but it ties the new backend to the old one's engine.

Two things now pull the other way. Production should run on PostgreSQL: it has
the stronger guarantees we lean on (transactional DDL for migrations over
immutable history, `SKIP LOCKED`, `RETURNING`, real booleans and time zones),
and it is what we want to operate. The end-to-end suite in `../webmail-e2e`
starts four or five Domains per run, each with its own database, and a MySQL
server per Domain makes that stack slow to start and heavy to run in parallel.
An SQLite file per Domain costs nothing to create and nothing to throw away.

The code today is written in MySQL's dialect: about 160 queries with backtick
identifiers, 57 calls to `NOW()`, 13 `INSERT IGNORE`, 4 `ON DUPLICATE KEY
UPDATE`, one `LastInsertId`, and the outbox claim with `FOR UPDATE SKIP LOCKED`.
None of that runs unchanged on PostgreSQL or SQLite.

## Decision

**Production runs PostgreSQL. Every test environment runs SQLite**, including
the end-to-end stack, CI and local development. MySQL is dropped from the Go
backend; it stays only where the PHP monolith still runs.

The application speaks **one portable SQL dialect** that both engines accept, so
the queries under test are the queries in production, character for character.
The differences are confined to a thin layer in `internal/store`: opening the
connection, rebinding placeholders, and engine settings. Full-text search is the
one deliberate exception and uses each engine's own index (below).

Drivers: `github.com/jackc/pgx/v5/stdlib` for PostgreSQL and `modernc.org/sqlite`
for SQLite. The latter is pure Go, so builds stay cgo-free and cross-compile as
they do today. `WEBMAIL_DSN` selects the engine by scheme (`postgres://` or
`sqlite://`).

### The portable dialect

| MySQL today | Portable form | Why it works on both |
| --- | --- | --- |
| `` `name` `` | `"name"` | ANSI identifier quoting; needed anyway because `user` is reserved in PostgreSQL |
| `?` placeholders | `?` in source, rebound to `$1..$n` for PostgreSQL | One spelling in the code |
| `NOW()` | the current time passed in from Go, in UTC | No engine clock in queries. This is also what lets tests move the clock (`49 hours pass`, `31 days pass`) |
| `INSERT IGNORE` | `INSERT ... ON CONFLICT DO NOTHING` | Supported by both. It only works against a real unique constraint, see below |
| `ON DUPLICATE KEY UPDATE x = VALUES(x)` | `ON CONFLICT (cols) DO UPDATE SET x = excluded.x` | Supported by both |
| `LastInsertId()` | `INSERT ... RETURNING id` | pgx has no `LastInsertId`; SQLite has `RETURNING` since 3.35 |
| `FOR UPDATE SKIP LOCKED` claim | lease: `UPDATE ... SET claimed_by = ?, claimed_until = ? WHERE id IN (SELECT ...) AND (claimed_until IS NULL OR claimed_until < ?) RETURNING ...` | One statement on both. The claim condition on the outer `UPDATE` is re-checked by PostgreSQL after a lock wait, so two workers cannot claim one row; SQLite has a single writer and needs nothing more |

Types come from the DBML, so the schema stays one file. `dbml-tools tosql`
renders auto-increment ids and enums per engine, but not column types: it
passes them through as written. `schema/webmail.dbml` was imported from MariaDB
and carries MySQL types (`bigint(20)`, `datetime`, `tinyint(1)`, `int(11)`,
`longtext`, `mediumtext`), which PostgreSQL rejects. The DBML is therefore
rewritten to portable types first: `bigint`, `integer`, `boolean`, `timestamp`,
`text`. SQLite output requires dbml-tools 0.9.7 or later.

### Full-text search stays engine-specific

Mail, contact and chat search use MySQL's `MATCH ... AGAINST (? IN BOOLEAN
MODE)`. No full-text syntax is shared by PostgreSQL and SQLite, and a generic
`LIKE` would change what users get (substring instead of word matching, no
operators), so search keeps a real full-text index on each engine:

| | PostgreSQL | SQLite |
| --- | --- | --- |
| Index | a stored generated `tsvector` column per searched column, with a GIN index | an FTS5 external-content table per searched table, kept current by triggers |
| Tokenising | the `simple` configuration: lowercase, no stemming, no stopwords | the `unicode61` tokenizer with `remove_diacritics 0` |
| Query | a `tsquery` built in Go | an FTS5 `MATCH` expression built in Go |

The searched columns are `g_thread.subject_line`, `g_message.body`,
`g_message.sender_name`, `contact.name`, `contact.email`,
`g_chat_message.body` and `g_chat_message.sender_name`.

The user's search string is parsed once, in Go, into a query that keeps today's
boolean-mode syntax: plain words are optional, `+word` is required, `-word` is
excluded, `"a phrase"` matches adjacent words and `word*` matches a prefix. Two
renderers turn that parsed query into a `tsquery` or an FTS5 expression. Users'
raw input never reaches either engine's query parser, so a stray quote or
operator cannot cause a syntax error. The `simple` configuration and `unicode61`
both lowercase and neither stems, so the two engines match the same words.
Unlike MySQL, neither drops words shorter than three characters or stopwords.

Search sits behind one interface in `internal/store` with an implementation per
engine, chosen when the connection opens. The callers in `internal/email` and
`internal/chat` build the rest of the query as before and get back a condition
and its arguments to add to their `WHERE`.

DBML cannot describe generated `tsvector` columns, GIN indexes or FTS5 tables
and triggers. They live next to the DBML in `schema/search.postgresql.sql` and
`schema/search.sqlite.sql`, and are applied after the tables. The MySQL
`FULLTEXT` indexes were never in the DBML either; they exist only in
`webmail.sql`.

### Rules the dialect cannot express

- **Email addresses are stored lowercase and compared lowercase.** MySQL's
  default collation compares case-insensitively and some lookups may depend on
  that. PostgreSQL and SQLite compare exactly. Normalising on write is the only
  rule that behaves the same on both.
- **Times are UTC**, as ADR 8 already requires. SQLite stores them as ISO 8601
  text; the store layer reads and writes `time.Time` in UTC on both engines.
- **SQLite runs in WAL mode with a busy timeout**, so a Domain's API and worker
  processes can share one file in the test stack.
- **SQLite connections set `PRAGMA foreign_keys = ON`.** SQLite stores the
  foreign keys but enforces them, cascades included, only on connections that
  ask. The store layer sets it on every connection it opens, so tests enforce
  the same references and `ON DELETE` actions as PostgreSQL.

## Consequences

- The end-to-end stack loses its database containers: each Domain is an API, a
  worker and an SQLite file in a volume. Stories can create Domains cheaply and
  run in parallel.
- The `g_subscription` unique index (ADR 17, known gaps) becomes a prerequisite
  rather than a nicety: `ON CONFLICT DO NOTHING` without a constraint inserts
  the duplicate. The production data cleanup has to happen first.
- The cutover is now also an engine move. ADR 14 compared both backends on one
  shared database; that is no longer possible. Parity is checked by migrating a
  copy of production from MySQL to PostgreSQL (pgloader for the data, the DBML
  for the schema) and comparing the PHP backend on the original with the Go
  backend on the copy, endpoint by endpoint. Row counts and checksums per table
  are compared before any response is.
- After the switch, writes live only in PostgreSQL. Rolling back to the PHP
  backend loses mail received after the switch, as the M2 cutover story already
  states.
- Tests and production run different engines. The portable dialect keeps the SQL
  identical, but it cannot prove PostgreSQL's locking, planner and type
  behaviour. A failure that only PostgreSQL produces would be found in
  production first. The recommended guard is to run the same end-to-end suite
  against PostgreSQL before each release; that is an open decision, not part of
  this ADR.
- Search is the one path where tests and production run different SQL. Tests on
  SQLite prove the shared parsing and the meaning of a search, not the
  PostgreSQL query or its index, which strengthens the case for that release
  check. Search needs its own unit tests that render one parsed query for both
  engines.
- Writes to searched tables also maintain the search index: a GIN update on
  PostgreSQL, a trigger on SQLite. Messages are append-only, so this is one extra
  index write per message.
- `CLAUDE.md`, `README.md`, `deploy/` and `TESTING.md` describe MySQL and need to
  change with the implementation, not before.
