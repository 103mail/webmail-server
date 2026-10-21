# webmail-server

Go backend for the webmail platform. It serves the JSON API defined by the
OpenAPI specification in [docs/openapi/](docs/openapi/) and replaces the PHP
monolith at `../webmail-legacy`. It runs on PostgreSQL in production and on SQLite in
every test environment (ADR 19), while the PHP backend runs on MySQL, so the
cutover moves the data as well. See [CLAUDE.md](CLAUDE.md) for the conventions
and [docs/adr/](docs/adr/) for the decisions behind them.

## Running

    go build ./cmd/webmail-server
    ./webmail-server

With no configuration it serves from `data/webmail.db`, an SQLite file it
creates, schema included, on first start.

For a full local stack instead, `deploy/` brings up four independent Domains
(103mail.com, example.com, mail.acme.test and acme.test) and the scripted test
peer peer.test, each with its own database, API process and hostname, serving
the client bundle from `../webmail-client`. They federate with each other:

    cd deploy && ./setup.sh && docker compose up -d --build

See [deploy/README.md](deploy/README.md) for the hosts entries it needs, the
test controls it carries, and what it does not do.

Configuration comes from the environment:

| Variable | Default | Meaning |
| --- | --- | --- |
| `WEBMAIL_ADDR` | `:8080` | Listen address |
| `WEBMAIL_DSN` | `sqlite://data/webmail.db` | Database: `postgres://user:pass@host/db` or `sqlite:///path/to/file.db` |
| `WEBMAIL_DATA_DIR` | `data` | Attachment and document bytes |
| `WEBMAIL_ALLOWED_ORIGINS` | localhost and 103mail.com | CORS origins, comma separated |
| `WEBMAIL_MAX_BODY_BYTES` | `33554432` | Request body cap |

Attachments are content-addressed by MD5 in the same four-level layout the PHP
backend uses, so its `data/attachments` directory carries over without copying.

## API

All endpoints live under `/api/v1/{group}/{entity}/{id}` and take a bearer
token, which `POST /api/v1/auth/token` issues from an email and password.

| Group | Entities |
| --- | --- |
| `auth` | `token` |
| `email` | `mailbox`, `folder`, `thread`, `subscriber`, `message`, `attachment`, `contact` |
| `calendar` | `calendar`, `meeting`, `attendee`, `document` |
| `chat` | `chat`, `participant`, `message`, `attachment`, `presence`, `typing`, `session` |

A `GET` reads its parameters from the query string; other methods accept a JSON
or form-encoded body. Attachments are nested objects, so a request carrying them
has to be JSON. `GET /health` needs no token.

Three endpoints sit outside that pattern and outside bearer authentication.
`GET /.well-known/webmail-federation` and `POST /api/v1/federation/inbound`
speak the federation protocol and are authenticated by Ed25519 signature
(ADR 17). `/wsoverhttp/{ClientId}` is the chat WebSocket bridge, authenticated
by the ClientId alone and therefore reachable only from inside the deployment
(ADR 18).

## Database

`schema/webmail.dbml` is the schema. `schema/generate.sh` renders it into
`webmail.postgresql.sql` and `webmail.sqlite.sql`, which are checked in and
embedded in the binary, and the full-text search indexes, which DBML cannot
describe, are in `search.postgresql.sql` and `search.sqlite.sql`. The server
applies both to an empty database when it starts, and leaves a database that
already has tables alone. `schema/webmail.sql` is the MySQL schema the PHP
backend runs on, kept for the cutover.

The code writes one SQL dialect both engines accept: ANSI quoted identifiers,
`?` placeholders, times passed in from Go, `ON CONFLICT` and `RETURNING`.
`internal/store` is the only place the engines differ. ADR 19 has the details,
including the rules the dialect cannot express: addresses are stored
lowercase, and times are UTC.

To run on PostgreSQL, create the role and an empty database, and point the
server at it:

    sudo -u postgres psql < create.sql
    WEBMAIL_DSN=postgres://webmail:webmail@localhost/webmail ./webmail-server

Schema changes to an existing database go through `dbml-tools migrate`, which
compares the live schema against `schema/webmail.dbml` and writes the SQL to
close the gap. Read what it generates before running it.

## Federation

Domains exchange mail over HTTPS rather than SMTP (ADR 11), and this implements
the Basic tier of the protocol in `../webmail-federation/project_rfc.md`: peer
discovery with trust on first use, request signing, inline attachments, and an
outbox that retries and bounces. The Registrar extension is not implemented.

| Variable | Meaning |
| --- | --- |
| `WEBMAIL_FEDERATION_CONFIG` | JSON file naming the Domains this Server hosts and their keys. Unset disables federation, and every recipient is treated as local |

`cmd/webmail-worker` drains the outbox. It is a separate binary because delivery
to a peer has a 72-hour retry budget and cannot sit in a request. ADR 17 has the
configuration shape and what is deliberately left out.

## Chat over WebSocket

Chat connections terminate in `../ws2api`, not here, so this tier stays
stateless (ADR 13). The proxy turns a connection into ordinary HTTP against
`/wsoverhttp/{ClientId}`, and the server pushes back through it.

| Variable | Meaning |
| --- | --- |
| `WEBMAIL_WS_PUSH_URL` | Where the ws2api tier listens. Empty disables fan-out: a message is stored and acknowledged, but the other participants do not see it until they reload |

`/wsoverhttp/` must never be exposed publicly: it is authenticated only by
knowing a ClientId. The reverse proxy in `deploy/` forwards `/api/` and `/ws/`
and nothing else. ADR 18 has the frame format.

## Email

`internal/mailer` moves email in and out. The two directions use different
services: outbound is published to an SNS topic, inbound comes from Amazon SES.
See ADR 15 for the transport and ADR 16 for what is sent and what is accepted.

**Outbound is notifications only.** `internal/notify` is the sole outbound path
and takes a template name and parameters, never a body, so message content
cannot leave by email (ADR 4). Content comes from an `email_template` row and
every attempt is recorded in `sent_email`. The rendered message is published to
an SNS topic and a subscriber delivers it; nothing here talks to a mail server.
Its first caller is external onboarding: a recipient with no account gets a
single-use token and the `invite` template.

**Inbound is accepted** at `POST /api/v1/mail/inbound`. An SES receipt rule
writes the raw message to S3 and announces it on an SNS topic, which posts here;
the server fetches the message, parses it and turns it into a thread. The
endpoint carries no bearer token; the SNS signature authenticates it, and
nothing is read from the body until that verifies. Messages SES flags for virus
or spam are dropped, mail from our own domains is ignored so it cannot loop, and
`inbound_email` makes redelivery idempotent.

Set these to turn email on. Each direction is independent, so either can run
alone:

| Variable | Meaning |
| --- | --- |
| `WEBMAIL_SNS_TOPIC_ARN` | Topic outbound notifications are published to. Empty records them in memory |
| `WEBMAIL_SNS_REGION` | Region of that topic. Empty means `WEBMAIL_SES_REGION` |
| `WEBMAIL_SES_REGION` | Region SES receives in and the S3 bucket lives in. Empty disables inbound |
| `WEBMAIL_SES_INBOUND_TOPIC_ARNS` | SNS topics inbound is accepted from |

With neither set the server stays off AWS: outbound is recorded in memory by
`mailer.Recorder` and the inbound webhook answers 503, so a development or
staging deployment cannot mail real people.

## Testing

[TESTING.md](TESTING.md) is the manual pass: how to set the local stack up,
what the seed data contains, and step by step checks for mail, calendar, chat
and federation. It is the real gate for chat, which has no automated coverage.

    go test ./...

The suite covers HTML to markdown conversion, recurrence expansion, the
day-view layout, content-addressed storage, request parsing, MIME rendering and
parsing with SNS signature verification, and the federation protocol's
signing, envelope validation and refusal rules. The domain packages are tested
against a real database: each test gets a fresh SQLite file with the full
schema, foreign keys enforced, from `internal/store/storetest`.

The same tests run against PostgreSQL when given a server to create their
schemas in. Each test gets a schema of its own, dropped when it ends:

    WEBMAIL_TEST_POSTGRES_DSN=postgres://postgres:secret@localhost/webmail_test go test ./...

That is the check that the portable dialect really is portable, and ADR 19
recommends running it before each release.

The port was checked by running this server and the PHP backend against the
same MySQL database and comparing responses: every shared endpoint returned
byte-identical JSON, with the calendar the sole exception because the PHP
implementation of it does not run (ADR 14). Now that the engines differ, the
same comparison is made between the PHP backend on MySQL and this one on a
PostgreSQL copy of the data, as ADR 19 describes for the cutover.
