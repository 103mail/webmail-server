# 15. Email transport: SNS out, SES in

Status: accepted
Date: 2026-08-24
Source: `../webmail` (`lib/MandrillApi.php`, `lib/EmailTemplates.php`)

## Context

The PHP backend sends mail through Mandrill. `EmailTemplates::send()` renders an
`email_template` row, hands it to `MandrillApi::sendFrom()`, and records the
result in `sent_email`. That is the whole mail story: there is no inbound path,
so a reply an external recipient types into their mail client is lost.

Two things have to be decided for the Go port. First, what carries outbound
notification mail, since Mandrill is a hand-rolled HTTP client against an API
that has been in maintenance for years. Second, whether inbound mail is accepted
at all, and if so how it reaches the backend.

Running our own SMTP daemon was rejected quickly. Listening on port 25 means
owning spam filtering, virus scanning, TLS, rate limiting and abuse handling, on
a port that is attacked continuously, for a feature peripheral to the product.
Deliverability for outbound is the same problem in reverse: IP warming and
reputation management are full-time work.

The two directions are not the same problem, though, and nothing says one
provider has to answer both. Outbound is a notification whose delivery we do not
want a user-facing request to wait for. Inbound is a stranger's message that has
to be scanned and authenticated before we will look at it.

## Decision

The two directions are decoupled and do not share a provider.

### Outbound is published to SNS

`internal/mailer.Topic` publishes each notification to an SNS topic as a JSON
`OutboundNotification` carrying sender, recipients, subject, the rendered bodies
and the rendered RFC 5322 message. Delivery is the subscriber's job. This
system's responsibility ends when the topic accepts the message.

Recipients travel in the payload rather than as SNS subscriptions. An SNS
subscription has to be confirmed by its owner before it receives anything, which
an onboarding invitation cannot satisfy: the whole point is to reach someone who
has no account and has agreed to nothing yet.

The SNS `Subject` field is left unset. It is limited to 100 ASCII characters
with no newlines, which a rendered subject line cannot be relied on to respect,
so the subject travels in the payload where it is not truncated. A `recipient`
message attribute is set so a subscription can filter without parsing the body.

Publishing rather than sending is also what keeps a user-facing action from
blocking on a mail provider, which is the rule ADR 11 states for federation and
which applies here for the same reason.

### Inbound comes from SES

An SES receipt rule accepts mail for our domains, scans it, writes the raw
message to S3 and publishes a notification to an SNS topic. SNS posts that
notification to `POST /api/v1/mail/inbound`, `mailer.Receiver` verifies the
signature, fetches the object and `ParseMIME` turns the bytes into an `Inbound`.

SES is the provider precisely because of what it does before we see a message.
It owns port 25, TLS and rate limiting, it runs the spam and virus scanners, and
it reports SPF, DKIM, DMARC, spam and virus verdicts with the message. A bare
MIME body posted to us carries none of that, and mail admitted into a thread
without it would let anyone post as anyone. The SNS signature authenticates the
post itself, so there is no shared secret of ours to distribute or rotate.

The receipt rule uses the S3 action rather than the SNS action. The SNS action
caps a message at 150 KB, which no message with attachments respects, and a
notification that only names a bucket and a key stays small however large the
message is.

Provider knowledge is confined to `Receiver`. `ParseMIME` takes raw RFC 5322
bytes and knows nothing about SES, so the parser, which is the bulk of the code
and all of the fiddly parts, survives a change of provider.

### Templates stay in the database

SES or SNS templating is not used. Bodies come from an `email_template` row as
they do in PHP, so an operator can change wording without a deploy.

### The SDK is pinned to Go 1.22

The AWS SDK modules are pinned to their last releases that build on Go 1.22,
which is what the target Debian machines ship. Moving the `go` directive to 1.24
is what unpins them. `aws-sdk-go-v2/service/sns` is held at v1.38.0 for this
reason; v1.39.21 and later require Go 1.24. Only `s3` and `sns` are needed:
nothing calls the SES API, since a receipt rule is configured in AWS rather than
from here.

## Consequences

- Nothing in this repo delivers outbound mail. The topic needs a
  subscriber that actually sends, and until one exists notification email does
  not arrive. That subscriber is a deployment component this repo does not
  contain.
- Decoupling means outbound failures are no longer visible here. A publish that
  succeeds says the topic accepted the notification, not that anyone received
  it; `sent_email.status` records the publish, not the delivery. Bounces and
  complaints are the subscriber's to surface.
- Receiving depends on three AWS services at once: a receipt rule, a bucket and
  a topic. That is the price of not operating SMTP, and it is paid in
  deployment steps rather than in code.
- The two directions now fail independently. Receiving can be broken while
  notifications still publish, and the other way round, so there are two things
  to monitor rather than one.
- Redelivery is expected, since SNS delivers at least once. Inbound is
  idempotent on the SES message id, which is assigned by the provider rather
  than chosen by the sender.
- Routing a reply into an existing thread needs a per-thread reply address, and
  the schema has no column for one. Inbound messages therefore start new
  threads. That is the remaining work before receiving is a feature rather than
  a capability.
