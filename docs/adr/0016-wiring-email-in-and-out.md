# 16. Wiring email in and out

Status: accepted
Date: 2026-08-24
Source: this repo; supersedes the "nothing imports this package" consequence of
[ADR 15](0015-email-transport-over-amazon-ses.md)

## Context

ADR 15 built email transport and left it with no caller. Connecting it raises
the question the transport itself deliberately does not answer: what may be
sent, and what happens to what arrives.

The rule from ADR 4 is that message content never leaves by email. A transport
type cannot enforce that, because `mailer.Outbound` has an `HTML` field and a
body is a body. The enforcement has to be in the shape of the path, not in a
convention.

Inbound is the opposite problem. A received message is a stranger's input that
becomes a thread, so the questions are what to reject, what not to ingest twice,
and what not to ingest at all.

## Decision

### Outbound is templates only

`internal/notify` is the only outbound path, and its `Send` takes a template
name and a parameter map. There is no argument through which a caller could
pass a message body. Content comes from an `email_template` row rendered with
`{{name}}` substitution, HTML-escaped, converted from markdown, and every
attempt is recorded in `sent_email`, matching `EmailTemplates::send`.

`email.Notifier` declares that signature as an interface in the mail domain, so
the domain never sees `mailer.Outbound` at all. To email thread content, someone
would have to add a new outbound path, which is a reviewable act rather than an
oversight.

The markdown source doubles as the plain-text alternative, so a client that will
not render HTML still receives a readable message.

### Onboarding is the first caller

Fan-out already creates an account for an unknown recipient. It now also mints a
single-use token valid for 30 days and, after the transaction commits, sends the
`invite` template carrying who wrote, the subject, and the token. That is ADR 6
made real.

The send happens after commit, never inside the transaction: a publish inside
one would hold locks across a network round trip and could not be rolled back.
The consequence is that delivery succeeds even when the invitation does not, so
a failed invitation is logged at error level rather than failing the request.

### Inbound is accepted, with a policy

`POST /api/v1/mail/inbound` takes the SNS post an SES receipt rule produces. It
carries no bearer token because the caller is Amazon; the SNS signature is the
authentication, and nothing is read from the body until it verifies.
Subscription confirmations are handled there too, since that is how the topic
gets wired up.

A message is then:

- **dropped** when SES reports a virus or spam verdict of FAIL, or when it has no
  sender or no recipient with a mailbox here;
- **ignored** when the sender's domain is one of ours, because that is our own
  outbound coming back and ingesting it would loop, which is the check
  `web/mail.php` makes;
- **ignored** when its provider id is already in `inbound_email`;
- otherwise **accepted** as a new thread with the sender and every recipient
  subscribed.

Envelope recipients come first and the To and Cc headers fill in the rest, since
the envelope is what SES accepted the message for.

Statuses are answered with 200 so SNS stops retrying. Only a genuine server
fault returns 5xx, which is what makes SNS retry, and a signature failure
returns 403 rather than 5xx so a forged post is not retried at all.

### Idempotency needs a table

`inbound_email` holds one row per provider message id, added by
`schema/migrations/0002-inbound-email.sql`. The insert is what claims the id, so
two concurrent redeliveries race on the primary key and exactly one wins. The
table is additive: no existing table changes, so the PHP backend is unaffected.

### Nothing is configured by default

The two directions are switched on separately. Without `WEBMAIL_SNS_TOPIC_ARN`
outbound goes to an in-memory `Recorder`, and without `WEBMAIL_SES_REGION` the
inbound webhook answers 503. A development or staging deployment therefore
cannot mail real people by accident, and either half can be turned on alone.

## Consequences

- Onboarding now emails people the PHP never emailed. `sent_email` is empty in
  the production database, so `EmailTemplates::send` was never called there.
  Turning this on in production is a behavioural change with an outside effect,
  and it is gated behind a configured topic and an `invite` template row,
  neither of which currently exists in production.
- An invitation is sent inline after commit, so a slow publish slows the
  request that triggered it. ADR 7's digest job wants a queue, and moving
  invitations onto it is the right fix when that exists.
- Message ordering across a redelivery is not preserved: a duplicate is dropped,
  but nothing reorders messages that arrive out of order.
- Dropping spam on SES's verdict means a false positive is lost silently. The
  drop is logged, which is the only trace.
- Inbound uses the same `deliver` fan-out as composing, so an inbound message
  can itself onboard recipients and trigger invitations.
