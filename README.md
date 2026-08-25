# webmail-server

Go backend for the webmail platform. It serves the JSON API defined by the
OpenAPI specification in `../webmail/web/openapi/v1/` and runs against the same
database as the PHP monolith at `../webmail`, so the two can be swapped or run
side by side. See [CLAUDE.md](CLAUDE.md) for the conventions and
[docs/adr/](docs/adr/) for the decisions behind them.

## Running

    go build ./cmd/webmail-server
    ./webmail-server

For a full local stack instead, `deploy/` brings up two independent Domains
(`webmail.103mail.com` and `webmail.example.com`), each with its own database,
API process and hostname, serving the client bundle from
`../webmail-client`:

    cd deploy && ./setup.sh && docker compose up -d --build

See [deploy/README.md](deploy/README.md) for the hosts entries it needs and for
what the topology does not yet do, federation above all.

Configuration comes from the environment:

| Variable | Default | Meaning |
| --- | --- | --- |
| `WEBMAIL_ADDR` | `:8080` | Listen address |
| `WEBMAIL_DSN` | `webmail:webmail@tcp(127.0.0.1:3306)/webmail` | MySQL connection |
| `WEBMAIL_DATA_DIR` | `data` | Attachment and document bytes |
| `WEBMAIL_ALLOWED_ORIGINS` | localhost and 103mail.com | CORS origins, comma separated |
| `WEBMAIL_MAX_BODY_BYTES` | `33554432` | Request body cap |

`WEBMAIL_DATA_DIR` must point at the same directory the PHP backend uses while
both are in service. Attachments are content-addressed by MD5 in the same
four-level layout, so they interoperate without copying.

## API

All endpoints live under `/api/v1/{group}/{entity}/{id}` and take a bearer
token, which `POST /api/v1/auth/token` issues from an email and password.

| Group | Entities |
| --- | --- |
| `auth` | `token` |
| `email` | `mailbox`, `folder`, `thread`, `subscriber`, `message`, `attachment`, `contact` |
| `calendar` | `calendar`, `meeting`, `attendee`, `document` |
| `chat` | `chat`, `participant`, `message`, `attachment`, `presence`, `typing` |

A `GET` reads its parameters from the query string; other methods accept a JSON
or form-encoded body. Attachments are nested objects, so a request carrying them
has to be JSON. `GET /health` needs no token.

## Database

`schema/webmail.sql` is the authoritative DDL and `schema/webmail.dbml` the
readable model of it. To create a database:

    mysql -u root -p < create.sql
    mysql -u webmail -p webmail < schema/webmail.sql

A database created before the calendaring work is brought up to date with
`dbml-tools migrate`, which compares the live schema against
`schema/webmail.dbml` and writes the SQL to close the gap. Read what it
generates before running it: the calendar work changes the type of
`g_attendee.will_attend`.

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

    go test ./...

The suite covers the logic that does not need a database: HTML to markdown
conversion, recurrence expansion, the day-view layout, content-addressed
storage, request parsing, and MIME rendering and parsing with SNS signature
verification.

For end-to-end verification, run this server and the PHP backend against the
same database and compare responses. That is how the port was checked: every
shared endpoint returns byte-identical JSON, with the calendar the sole
exception because the PHP implementation of it does not run. See ADR 14.
