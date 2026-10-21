# webmail-server

Go backend for the webmail platform. It replaces the server side of the PHP
monolith at `../webmail-legacy` and serves the API the client runs on. It also
implements the Basic tier of the federation RFC (ADR 17) and advertises
`registrar: false`; the Registrar tier is not built yet. 103mail.com production
still runs on PHP until the cutover.

## Related repositories

- `../webmail-legacy` is the PHP monolith (MintyPHP) that currently holds both
  the frontend and the backend and runs 103mail.com production. It is the reference for behaviour, schema and API
  shape, and it is being superseded. Read it, do not extend it.
- `../webmail-client` is the TypeScript SPA (React 19, MUI 9, Vite) and the only
  frontend. It runs on this API, configured per Domain through `/config.json`,
  with chat over WebSocket.
- `../webmail-federation` holds the protocol design for exchanging messages
  between independently operated domains. `project_rfc.md` is normative for
  anything cross-domain, `project_design.md` covers the migration,
  `project_overview.md` is the summary and `project_plan.md` the order of work
  and current status.
- `../webmail-e2e` holds the end-to-end stories, run with godog against the
  Domains in `deploy/` through the public API only. A milestone is done when its
  stories are green. The read-only controls it uses are in ADR 20.
- `../../dbml-tools` is the CLI used to keep the schema in DBML and to generate SQL
  and migrations from it.
- `../../ws2api` is the WebSocket proxy that terminates chat connections and turns
  them into plain HTTP against this backend, keeping the API server stateless.
  See ADR 13.

## Scope

- HTTP JSON API for auth, mail, calendar, chat and attachments, replacing
  `pages/api/v1/` and `lib/Public/` in the PHP repo.
- `deploy/` runs four independent Domains locally (103mail.com, example.com,
  mail.acme.test and its parent acme.test) that federate with each other, plus
  the scripted test peer peer.test, a fault proxy and the test controls the
  end-to-end stories use; see `deploy/README.md`.
- Federation endpoints from the RFC: `GET /.well-known/webmail-federation` and
  `POST /api/v1/federation/inbound`.
- Background workers for scheduled work: digests, reminders, retention purges and
  federation retries.
- Email out over SNS and in from Amazon SES (ADR 15): `internal/mailer` for
  transport, `internal/notify` for outbound notifications, and
  `POST /api/v1/mail/inbound` for received mail. Outbound can only send an
  `email_template` row, which is what keeps message content off email.
- No HTML rendering and no server-side templates. The client is a separate SPA.
  The only rendered output is notification email built from `email_template`
  rows.

## Principles

Carried over from `../webmail-legacy/.specify/memory/constitution.md` (v1.0.3) and
adapted to Go. See `docs/adr/` for the reasoning behind each.

1. **Content privacy and non-forwarding.** Message content never leaves the
   system by email. Threads cannot be forwarded. External recipients onboard via
   content-free notification email and a secure link. Quoting creates a new
   independent thread with no backlink.
2. **Immutable message history.** Messages are append-only. Users cannot delete
   or rewrite sent content. Retention purges are controlled, logged, and never
   silently mutate history.
3. **Projection-based access control.** Canonical objects (`g_thread`,
   `g_message`, `g_attachment`, `g_subscription`, `g_chat*`, `g_meeting`) are
   immutable. Per-user projections carry read and deleted state. All visibility,
   search and permission checks derive from projections plus subscription
   membership.
4. **Test first, Given/When/Then acceptance.** Every change starts from a
   scenario and a failing test. Unit for logic, integration for projection rules,
   end-to-end for user flows.
5. **Scheduled and deferred operations.** Digests, retention sweeps, reminders
   and federation retries run in workers outside the request path. They are
   idempotent, record execution markers for observability, and interactive
   features never block on them.
6. **Minimal, coalesced notifications.** In-app notification is immediate. Email
   is a digest at the user's configured frequency, only when unread items exist,
   strictly content-free, with a secure deep link to the unread view.
7. **Secure deep links and token hygiene.** Every email link is tokenised,
   time-bound, scoped to a single user and non-guessable. No content in URL
   parameters beyond an opaque token and minimal routing metadata.
8. **Performance and clarity.** Search returns within 1s for typical workloads.
   Filtered contexts always report that results are partial and offer one-click
   restoration.
9. **Schema naming.** Table names are singular snake_case, canonical tables
   prefixed `g_`. Plural table names are disallowed.
10. **Package boundaries.** Domain logic lives in `internal/` packages with no
    knowledge of HTTP. Handlers translate, they do not decide. No package-level
    mutable state: configuration and dependencies are passed in explicitly.

## Go conventions

- Module path `github.com/mevdschee/webmail-server`, matching the sibling Go
  repos.
- `cmd/` holds binaries (API server, workers). `internal/` holds everything else.
  `pkg/` only for code genuinely meant for outside consumers.
- Standard library first. Only proven dependencies; `database/sql` with
  `jackc/pgx/v5` and `modernc.org/sqlite` rather than an ORM. Both drivers are
  pure Go, so builds stay cgo-free.
- Wrap errors with `%w` and handle them at the boundary. No panics in request or
  worker paths.
- `gofmt` and `go vet` clean before commit. This is the equivalent of the PHP
  repo's phpstan level 5 gate.
- Document per folder why it exists and how it relates to the rest of the repo.

## Database

- PostgreSQL in production, SQLite in every test environment (ADR 19). Singular
  snake_case tables, canonical `g_*` tables plus per-user projections, matching
  the PHP schema so its data can be migrated.
- Write the one portable dialect both engines accept: `"quoted"` identifiers,
  `?` placeholders, the current time from `db.Now()` rather than `NOW()`,
  `ON CONFLICT` instead of `INSERT IGNORE` and `ON DUPLICATE KEY`, `RETURNING`
  (via `store.Insert`) instead of `LastInsertId`, `TRUE`/`FALSE` for booleans,
  and no `LIMIT` on `UPDATE` or `DELETE`. Engine differences live only in
  `internal/store`, full-text search included (`db.Match`).
- Store and compare email addresses lowercase (`store.Address`).
- Keep the schema as DBML and generate SQL and migrations with `../../dbml-tools`
  (`dbml-tools check`, `tosql`, `migrate`); `schema/generate.sh` renders both
  engines' DDL.
- Times are stored in UTC. IANA timezone identifiers are stored alongside for
  display only.

## Testing

- `go test ./...` must pass. Mirror the PHP layout: unit tests for logic,
  integration tests against a real database for projection rules
  (`internal/store/storetest`, SQLite by default, PostgreSQL with
  `WEBMAIL_TEST_POSTGRES_DSN`), end-to-end stories in `../webmail-e2e`.
- External services (mail providers, federation peers) must be mockable so tests
  stay deterministic.
- Cover every exported function of a package before merging it.

## Documentation

- `docs/adr/` records architecture decisions. Add an ADR when a decision changes
  rather than editing history.
- Maintain an OpenAPI spec for the API, as the PHP repo does under
  `web/openapi/`.
- Maintain installation and operation notes for Debian based servers.
