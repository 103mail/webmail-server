# Testing

How to bring the local stack up and check by hand that mail, calendar and chat
actually work. The stack is the one in [deploy/](deploy/): two independent
Domains, each with its own SQLite database, API process, worker, chat proxy
and hostname. Testing against it is the closest thing to production short of
deploying, because it exercises the real client bundle, the real reverse proxy
and the real WebSocket path rather than a mock.

Automated tests are a separate and much smaller thing. They are at the end,
under [Automated tests](#automated-tests).

## Before you start

| Needed | Why |
| --- | --- |
| Docker with the compose plugin | Everything runs in containers |
| `openssl` | `setup.sh` generates the CA, the leaf certificates and the federation keys |
| `sqlite3` and `python3` | `setup.sh` builds each Domain's database with them |
| `../webmail-client` | The client bundle is built from it |
| `../ws2api` | The chat proxy is built from it |
| A local `webmail` MySQL database | Optional. Seeds the 103mail Domain with real data |
| `python3` with `websockets` | Optional. Only for `deploy/chatcheck.py` |

The sibling repositories have to be checked out next to this one, because
compose builds them by relative path.

## Setting up

    cd deploy
    ./setup.sh

`setup.sh` is idempotent and generates the things that cannot be checked in:

- a local CA, plus a leaf certificate for `webmail.103mail.com`,
  `webmail.example.com`, `webmail.mail.acme.test` and `webmail.acme.test`
- an Ed25519 federation keypair per Domain, and the `federation.json` that
  names it
- the seed SQL for every Domain
- the attachment and document directories

Re-running keeps what already exists, so the CA you trusted stays trusted and
the peer keys other Domains pinned do not move. Use `./setup.sh --reseed` to
rebuild only the seed SQL.

Then, once per machine:

1. Add the Federation hostnames to `/etc/hosts`. All of `127.0.0.0/8` is
   loopback on Linux, which is what lets every Domain own port 443 and keep
   URLs with no port suffix:

       127.0.0.1  webmail.103mail.com
       127.0.0.2  webmail.example.com
       127.0.0.3  webmail.mail.acme.test
       127.0.0.4  webmail.acme.test

2. Trust `deploy/certs/webmail-local-ca.crt` in the browser. Chrome: Settings,
   Privacy and security, Security, Manage certificates, Authorities, Import.
   Firefox has its own store under Settings, Privacy & Security, Certificates,
   View Certificates, Authorities. Without this every page load is a warning,
   and the client's fetches fail rather than prompting.

3. Bring it up:

       docker compose up -d --build

Fifteen containers should come up: an API, a worker and an nginx per Domain,
a chat proxy for 103mail.com and example.com, and the mail sink. There is no database container; each Domain's database is the file
`deploy/data/{domain}/webmail.db`, which `setup.sh` built. `docker compose ps` shows them.

## What the seed data contains

The two Domains are seeded differently on purpose.

| | 103mail.com | example.com |
| --- | --- | --- |
| URL | `https://webmail.103mail.com` | `https://webmail.example.com` |
| Loopback | `127.0.0.1` | `127.0.0.2` |
| Seed | data-only dump of your local `webmail` MySQL database, converted for SQLite | fixtures from `deploy/seed.sh` |
| Logins | whatever that database holds | `bob@example.com`, `carol@example.com` |
| Password | `password` | `password` |

Every account uses `password`, which is what `../webmail/syncdata.sh` sets on a
synced database. If you have no local `webmail` database, `setup.sh` generates
the same fixtures for 103mail.com too, with `maurits@vdschee.nl` as the login.

**Log in with a username, not an alias.** `internal/auth` looks the account up
by `user.username` and never consults `alias`, so `mevdschee@103mail.com` is
not a login even though mail addressed to it arrives fine. It is an alias on
the same mailbox as `maurits@vdschee.nl`, which is the login. A wrong address
here answers `403` with a `null` body, which looks like an authorization
failure rather than a typo. To see what a Domain will accept:

    sqlite3 data/103mail.com/webmail.db 'SELECT id, username FROM "user" ORDER BY id LIMIT 10'

Addresses are stored and compared in lowercase (ADR 19), so the case you type
a login in does not matter.

The generated fixtures give each Domain one Customer per account, and per
account a mailbox (which is also that account's calendar), an Inbox folder and
an alias. On top of that:

- **Mail.** One thread, `Welcome to {domain}`, with two messages, the first
  unread. The other party is on the other Domain, so the fixtures read like a
  federated deployment.
- **Calendar.** Three meetings placed relative to today, so the default view is
  never empty: `Standup` today, `Design review` tomorrow, and `Company day` in
  three days as an all-day event. RSVP is `yes`, `maybe` and unanswered
  respectively.
- **Chat.** Two direct chats. One with the peer on the other Domain, which has
  history but cannot go live because chat is not federated. One between the two
  local accounts, which is the one to test live.

Two accounts rather than one because a live chat needs two people who can both
sign in, and chat is not federated, so they have to share a Domain. A 103mail
Domain seeded from a real dump has whatever accounts that database holds
instead.

## Smoke test before opening the browser

Four checks, in order, so a failure tells you which tier is wrong. Run them
from `deploy/`.

    # 1. nginx is up and TLS is trusted
    curl --cacert certs/webmail-local-ca.crt https://webmail.example.com/health

Expect `{"status":"ok"}`. This is proxied to the API, so it also proves nginx
can reach the API container and the API can answer.

    # 2. the client will know which Domain it is
    curl --cacert certs/webmail-local-ca.crt https://webmail.example.com/config.json

Expect the Domain's branding as JSON. A `404` here is the failure behind the
client's "This domain is not configured" screen. See
[Troubleshooting](#troubleshooting).

    # 3. credentials work
    curl --cacert certs/webmail-local-ca.crt \
      -X POST https://webmail.example.com/api/v1/auth/token \
      -H 'Content-Type: application/json' \
      -d '{"email":"bob@example.com","password":"password"}'

Expect `{"name":"Session","value":"..."}`. Keep that value; the API sections
below use it as `$TOK`.

    # 4. the federation descriptor is signed and served
    curl --cacert certs/webmail-local-ca.crt \
      https://webmail.example.com/.well-known/webmail-federation

Expect a JSON descriptor with the Domain's public key. It is rendered from the
key the server signs with, so if this answers, the key file is readable and
loaded.

Only when all four pass is a browser failure worth debugging as a client
problem.

## Manual test: mail

Sign in at `https://webmail.example.com` as `bob@example.com` / `password`.
Mail is the section the client opens on.

1. **The thread list loads.** The Inbox shows `Welcome to example.com`, with the
   subject in bold and an unread count, because the first message is unread and
   the second is not. An empty list means the seed did not load; rebuild
   the database with `./setup.sh --reseed` and read what it reports.

2. **Reading marks read.** Open the thread. Both messages render, oldest first,
   the older one from the peer Domain and the newer from bob. The unread mark
   clears and the folder's unread count drops. Go back to the list and confirm
   it stays cleared, which is the projection being written rather than component
   state.

3. **Reply.** With the thread open, use the reply button. Send a short body. It
   appears at the bottom of the thread immediately and survives a reload.
   History is append-only, so there is no edit or delete on a sent message, and
   that absence is correct rather than missing.

4. **Compose a new thread.** Use the compose button. Fill `To` (the field
   autocompletes from contacts, so `carol@example.com` should be suggested),
   `Subject`, and a body, then send. The new thread appears at the top of the
   Inbox.

5. **"Forward" adds a subscriber rather than forwarding.** With a thread open,
   use the forward action in the toolbar and pick a contact. Nothing is sent
   anywhere and no message is appended: the contact is added to the thread's
   subscription and sees it through their own projection from then on, while the
   history stays where it is. Confirm the subscriber list on the thread grew and
   the message count did not. The absence of a real forward is principle 1, not
   a missing feature.

6. **Attachments.** Attach a file to a message and send it. Reopen the thread
   and download it back. Bytes are content-addressed by MD5 under
   `deploy/data/example.com/attachments` in a four-level layout, so a second
   copy of the same file adds a row and no bytes:

       find data/example.com/attachments -type f | wc -l

7. **Move and delete.** Select a thread from the list and use the move and
   delete actions in the toolbar. Delete sets the projection's deleted state; it
   does not remove the canonical thread, which is what makes the deletion
   per-user rather than destructive.

8. **Search.** Search from the toolbar. Results are scoped to the current
   folder, and the list must say so rather than silently showing a subset: a
   banner reading `Showing results for '...'` appears above it, with a clear
   button that restores the full folder in one click. A filtered list with no
   banner is a bug, because it is indistinguishable from an empty folder. That
   is principle 8.

## Manual test: calendar

Switch to Calendar in the left rail. The seeded meetings sit around today, so
the default view has content.

1. **The three seeded meetings appear.** `Standup` today at 09:00, `Design
   review` tomorrow at 10:00, both Amsterdam local, and `Company day` in three
   days rendered as an all-day band rather than a timed block.

   The two timed ones are stored as the UTC instant and displayed in the
   browser's zone, so in Amsterdam they read 09:00 and 10:00 and elsewhere they
   read whatever that instant is locally. The all-day one is stored midnight to
   midnight UTC and must occupy exactly one day; drawn across two, the seed
   converted it when it should not have.

2. **The views agree.** Cycle through the view menu in the toolbar. Depending
   on window width you get day, 3-day, 5-day, 7-day, month and agenda. Every
   view should show the same three meetings. The toolbar's today, previous and
   next controls move the anchor without losing them.

3. **Overlap layout.** Create a meeting that overlaps `Standup`. Both should
   render side by side at half width rather than on top of each other. The
   server computes this: `widthFraction` and `horizontalPosition` come back on
   each meeting from `GET /api/v1/calendar/meeting/{calendarIds}`, so a layout
   bug is worth checking against the response before blaming the client.

4. **Create.** Use the event dialog to add a meeting with a title, start and
   end, and a description. It should appear immediately and survive a reload.
   Times are stored UTC with the IANA zone alongside for display, so a meeting
   created at 10:00 Amsterdam is stored as 08:00 UTC. Check that the stored
   value is UTC and the displayed value is local:

       sqlite3 data/example.com/webmail.db \
         "SELECT id, title, start_time, time_zone FROM g_meeting ORDER BY id DESC LIMIT 3"

5. **All-day.** Create one with all-day set. It renders in the all-day band and
   does not occupy a time slot.

6. **Delete.** Delete a meeting from the dialog and confirm it leaves every
   view.

7. **Recurrence.** Recurrence expansion is server-side (`internal/rrule`). The
   dialog does not offer a repeat rule, so a recurring meeting has to be created
   through the API to be exercised in the UI:

       DAY=$(date -d tomorrow +%Y-%m-%d)
       curl --cacert certs/webmail-local-ca.crt \
         -X POST https://webmail.example.com/api/v1/calendar/meeting/1 \
         -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
         -d "{\"title\":\"Weekly\",\"startTime\":\"${DAY}T10:00:00\",
              \"endTime\":\"${DAY}T11:00:00\",\"attendees\":[],
              \"timeZone\":\"Europe/Amsterdam\",\"repeatRule\":\"FREQ=WEEKLY;COUNT=4\"}"

   The date is computed rather than written out, because a meeting seeded into
   the past falls outside the default window and looks like a failure. Then
   confirm four occurrences appear on four consecutive weeks in the month view.
   The response carries every occurrence, so it can be checked without the UI:

       curl --cacert certs/webmail-local-ca.crt -H "Authorization: Bearer $TOK" \
         "https://webmail.example.com/api/v1/calendar/meeting/1?untilDate=$(date -d '+6 weeks' +%Y-%m-%d)"

## Manual test: chat

Chat is the only section that needs two identities at once, and the only one
whose transport is not plain HTTP. Connections terminate in the `ws-*`
containers, never in the API, which is what keeps the API tier stateless.

Use two browsers, or one normal window and one private window. A second profile
works; a second tab in the same profile does not, because it shares the session.

1. **Sign both in.** `bob@example.com` in one and `carol@example.com` in the
   other, both at `https://webmail.example.com`. Open Chat in each and select
   the direct chat between them. Both should show the seeded message.

2. **A message arrives without a reload.** Type in bob's window and send. It
   appears in carol's within a moment, with no refresh. There is no polling
   fallback any more, so if it does not arrive it will not arrive late either:
   it stays invisible until carol reloads. That is the signature of a socket
   that is down or of fan-out being disabled, and step 7 tells the two apart.

3. **The typing indicator shows.** Start typing in bob's window without sending.
   Carol's header shows that bob is typing, and it clears when he stops. Typing
   has an `expires_at`, so it clears on its own even if the stop frame is lost.

4. **Presence.** Closing bob's window should turn him offline in carol's view.
   The proxy calls `DELETE` on close, which makes the common case immediate;
   presence also has a TTL on `last_activity_at`, so a proxy that dies without
   closing still resolves, just more slowly.

5. **Attachments.** Attach a file or an image from the composer. It should
   arrive in the other window and open from there.

6. **Edit and delete.** Unlike mail, a chat message can be edited and deleted.
   Both should reflect in the other window without a reload.

7. **The proxy agrees with what you saw.** Counters are on the proxy's own port,
   reachable only from inside the Domain network:

       docker compose exec api-example wget -qO- http://ws:7001/

   `connections_opened` goes up by one per window, `connections_closed`
   trails it, and `connections_denied` should stay at `0`. A non-zero denied
   count means two connections claimed the same ClientId, which is refused
   because a duplicate used to silently stop the first socket receiving pushes.
   `requests_failed` should stay at `0`.

### Checking chat without two browsers

`deploy/chatcheck.py` drives both sides over real sockets and prints what each
one saw. It signs both accounts in, mints a ClientId each, opens both sockets
and checks that a typing frame and a message frame cross:

    cd deploy && python3 chatcheck.py

It resolves the hostname to the loopback address itself, so it runs whether or
not `/etc/hosts` is set up. Needs `python3` with the `websockets` package.

Expect four lines: both sides ready, the typing frame seen by the other, and the
message seen by the other.

One thing that surprises people reading the output: the two sides report
different `chatId` values for the same conversation. That is correct. Each user
reads their own projection row, so bob's chat 2 and carol's chat 3 are the same
canonical `g_chat`. Do not report it as a bug.

## Manual test: federation

Mail between Domains travels over HTTPS rather than SMTP, signed with each
Domain's Ed25519 key. Both `web-*` containers carry their real Federation
hostname as an alias on the shared `federation` network, so one API resolves the
other exactly as it would on the internet.

1. Signed in as `bob@example.com`, compose to `mevdschee@103mail.com`. The
   thread is written locally and the browser returns immediately, because
   delivery to a peer has a 72-hour retry budget and cannot sit in a request.

2. Watch the outbox drain:

       sqlite3 data/example.com/webmail.db \
         "SELECT peer_domain, status, attempts, last_http_status FROM federation_outbox"
       docker compose logs -f worker-example

   Status should reach delivered with `last_http_status` 200.

3. Sign in on `https://webmail.103mail.com` as that Domain's login and confirm
   the thread arrived in the real mailbox. Reply, and confirm the reply comes
   back onto the same thread on the example.com side.

Only the Basic tier is implemented. The Registrar extension is not, and both
Domains advertise `"registrar": false`.

## What the client cannot exercise yet

Several endpoints work but have no caller in the client, so they can only be
tested with `curl`. Checked against `../webmail-client/src`; worth re-checking
if the client has moved on.

| Endpoint | State |
| --- | --- |
| `PUT /calendar/meeting/{id}` with `willAttend` | No UI. RSVP is neither set nor displayed |
| `POST /calendar/attendee/{meetingId}` | No UI, and the dialog posts `attendees: []` |
| `POST`, `GET`, `DELETE /calendar/document/{id}` | No UI at all |
| `POST /email/contact/{customerId}` | Contacts are read for autocomplete, never added |
| `POST /chat/chat/{mailboxId}` | No UI for starting a new chat |
| `PUT /chat/chat/{id}` | Mute is local component state and is not persisted |

The REST `chat/typing` and `chat/presence` endpoints have no caller either, but
that one is deliberate rather than a gap: the socket carries typing and presence
now, and the REST pair is what it replaced.

An RSVP, for example, has to be set and read back over the API, because the
calendar neither offers a control for it nor renders the answer:

    curl --cacert certs/webmail-local-ca.crt \
      -X PUT https://webmail.example.com/api/v1/calendar/meeting/2 \
      -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
      -d '{"willAttend":"yes"}'

    curl --cacert certs/webmail-local-ca.crt -H "Authorization: Bearer $TOK" \
      https://webmail.example.com/api/v1/calendar/meeting/1 | grep -o '"willAttend":"[a-z]*"'

Expect `true` from the first and the new answer in the second. Testing this
through the browser is not possible today.

## Testing against the API directly

Every endpoint is `/api/v1/{group}/{entity}/{id}` with a bearer token. The `id`
is not always the entity's own id, which is the single most common mistake when
driving it by hand:

| Request | What `{id}` means |
| --- | --- |
| `GET /email/mailbox/{id}` | the customer id |
| `GET /email/folder/{id}` | the mailbox id |
| `GET /email/thread/{id}` | the folder id |
| `GET /email/message/{id}` | the thread id |
| `POST /email/thread/{id}` | the folder to create it in |
| `POST /email/message/{id}` | the thread to reply to |
| `GET /email/contact/{id}` | the customer id |
| `GET /calendar/calendar/{id}` | the customer id |
| `GET /calendar/meeting/{id}` | calendar ids, comma separated |
| `PUT`, `DELETE /calendar/meeting/{id}` | the meeting id |
| `GET /chat/chat/{id}` | the mailbox id |
| `GET /chat/message/{id}` | the chat id |
| `GET /chat/presence/{id}` | user ids, comma separated |

A `GET` reads its parameters from the query string; everything else takes a JSON
or form-encoded body. Attachments are nested objects, so a request carrying them
has to be JSON. On the generated fixtures every one of these ids is `1` for the
first account, which makes a walkthrough easy:

    curl --cacert certs/webmail-local-ca.crt \
      -H "Authorization: Bearer $TOK" \
      https://webmail.example.com/api/v1/email/thread/1

The 103mail Domain is a dump of a real database, so its ids are whatever that
database holds. Read them from `GET /email/mailbox/{customerId}` and work down.

## Resetting between runs

Manual testing writes rows. To get back to a known state, rebuild the seed SQL
and the databases with the stack stopped:

    docker compose down
    ./setup.sh --reseed
    docker compose up -d

The databases are files, so `down` on its own keeps everything you created. To
reset one Domain only, delete its `data/{domain}/webmail.db` and run
`./setup.sh`, which builds whatever database is missing.

To change what the fixtures contain, edit `deploy/seed.sh` and reseed. Never
edit `seed/example.com.sqlite.sql` directly. It is generated, and `--reseed`
overwrites it.

`seed/103mail.com.sqlite.sql` is converted from a `mysqldump` of your local
`webmail` database taken at setup time. `--reseed` re-dumps it, which is how
you pull in newer data from the PHP side.

## Troubleshooting

**"This domain is not configured", `config.json: 404`.** nginx is serving from a
bind mount that no longer exists. A bind mount is pinned to the inode it
resolved when the container started, and a `git checkout`, rebase or stash
replaces files by writing new ones and renaming, which changes inodes. The
containers keep mounting the old, now-unlinked directory and it reads as empty.
Compare them:

    stat -c '%i %n' domain/103mail.com
    docker compose exec web-103mail stat -c '%i %n' /etc/nginx/domain

Different numbers confirm it. Recreating re-resolves the mount, and `restart`
does not, because it reuses the existing container:

    docker compose up -d --force-recreate --no-deps web-103mail web-example

`--no-deps` leaves the other containers alone. The same applies after any branch switch while the stack is up. The `api-*` and
`worker-*` containers mount `federation.json` as a single file rather than a
directory, so they fail silently instead: they go on serving the old file's
contents with no error. Recreate them too if the file changed.

**`403` from `POST /api/v1/auth/token`.** Bad credentials, not CORS. See the
username and alias note under [What the seed data
contains](#what-the-seed-data-contains).

**Chat messages need a reload to appear.** Fan-out is off. `WEBMAIL_WS_PUSH_URL`
must point at the Domain's proxy; unset, a message is stored and acknowledged
but never pushed. The server logs a warning at startup rather than failing,
because a deployment without chat is legitimate. Check the proxy is reachable
from the API:

    docker compose exec api-example wget -qO- http://ws:7001/

**One browser stops receiving pushes.** Check `connections_denied` on that
proxy. Two connections claiming the same ClientId is refused now, but a client
that reuses one will lose its socket. A ClientId is single-use for connecting.

**Certificate warnings in the browser.** The CA is not trusted, or it was
regenerated. `setup.sh` keeps an existing CA precisely so this does not happen;
if you deleted `certs/`, re-import the new `webmail-local-ca.crt`.

**A peer refuses the certificate.** Correct behaviour. RFC 9.2 requires a Web
PKI certificate, and these are signed by a local CA. Nothing to work around
locally.

**Email never arrives at a real address.** By design. Outbound is published
to the mail sink instead of SNS, and the inbound webhook answers `503`. The
local stack cannot mail a real person. To see what would have been sent, such
as an invitation, read `curl http://127.0.0.1:8025/messages`.

## Automated tests

    go test ./...

The suite covers HTML to markdown conversion, recurrence expansion, the
day-view layout, content-addressed storage, request parsing, MIME rendering and
parsing with SNS signature verification, and the federation protocol's
signing, envelope validation and refusal rules. The domain packages (`auth`,
`email`, `calendar`, `chat`, `notify`, `federation`) are tested against a real
SQLite database per test, which covers the projection rules, search, the chat
session claim and the outbox leases.

The same tests run against PostgreSQL, the production engine, when given a
server to create a schema per test in (ADR 19):

    WEBMAIL_TEST_POSTGRES_DSN=postgres://postgres:secret@localhost/webmail_test go test ./...

`internal/config` and `internal/wsbridge` still have no tests, and the socket
path is not covered, which is why the manual passes above are the real gate for
chat in particular.

Before committing, the same gate the PHP repo applies at phpstan level 5:

    gofmt -l .
    go vet ./...

Both should print nothing.

For verification against the implementation being replaced, run the PHP
backend on its MySQL database and this server on a PostgreSQL copy of it, and
compare responses (ADR 19). Every shared endpoint should return byte-identical
JSON, with the calendar the sole exception because the PHP implementation of it
does not run.
