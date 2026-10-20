# 17. Implementing Webmail-Federation

Status: accepted
Date: 2026-08-25
Source: `../webmail-federation/project_rfc.md` (v0.2, normative),
`project_design.md`. Implements the decision recorded in ADR 11.

## Context

ADR 11 decided that Domains exchange messages over HTTPS. The RFC specifies the
wire; this ADR records how it is built here and the choices the RFC leaves open.
A fan-out that treats every recipient as local silently loses mail: a message to
`bob@example.com` creates a local account with a `103mail.com` alias, and the
sender sees a Sent copy and believes it arrived.

## Decision

Implement the **Basic Webmail-Federation Domain** tier: peer discovery (RFC 4),
envelope format (5), inbound transfer (6), inline attachments (7), transport,
auth scope, replay and rotation (9.1–9.5, 9.8), and versioning (10). The
Registrar extension (RFC 8) and everything that depends on it (shadow
mailboxes, real-email addressing, multi-registrar coordination) is out of
scope and this Domain advertises `"registrar": false`.

### Shape

```
internal/federation      the wire, and nothing else
  envelope.go   envelope schema and every field rule, as pure validation
  sign.go       Ed25519 signing input, sign, verify, key and date encoding
  descriptor.go peer descriptor, and the discovery fetch
  federation.go local Domain registry, peer cache, TLS, configuration
  outbox.go     enqueue, claim, deliver, retry, bounce, sweep
  inbound.go    verify, dedupe, decode

internal/email           the mail domain, unchanged in shape
  federate.go   the outbound half of the fan-out split
  receive.go    writing an accepted envelope, and bouncing a failed one

internal/api/federation.go   two handlers that translate and decide nothing
cmd/webmail-worker           drains the outbox, outside the request path
```

**No package imports both directions.** `internal/federation` never imports
`internal/email`. It hands accepted envelopes to a `Deliverer` interface it
declares itself, and `*email.Service` satisfies it. The mail domain declares a
`Federator` interface for what it needs and exchanges federation-owned types
across it. The two are wired in `main`, which is the only place that knows
both. This is principle 10 applied to a pair of services that genuinely refer
to each other; the construction cycle is broken by one setter, `Attach`.

**Protocol rules live in the domain package, not the handler.** Every refusal,
including which failures are permanent, is decided in `internal/federation` and
returned as a `Rejection` carrying an HTTP status and a wire error code. The
handlers translate. That is what makes the whole verification path testable
without an HTTP server, and it is the same discipline the SES webhook follows.

### Fan-out splits, and the canonical rows do not

`deliver` sorts each subscriber into local or remote. A local subscriber
gets the projection row it always got. A remote subscriber gets an envelope
queued for their Domain, and **no local account**: creating one would hand them
an address here that is not theirs. They become a `contact` row so autocomplete
still works.

`g_thread`, `g_message` and `g_attachment` are still written once, locally, so
the sender keeps their Sent copy. Only the per-recipient projection is replaced
by a network hop. A federated message is not a second class of message; the one
difference is that the canonical rows carry the identifiers the origin chose.

### Federated identifiers

`g_thread`, `g_message` and `g_attachment` gain a `federation_id` (UUIDv7,
unique, nullable). Local autoincrement ids cannot correlate anything across two
databases; these can, and they are what puts a reply from a peer on the thread
it belongs to rather than starting a new one.

They are minted **lazily**, the first time a row needs to cross a boundary.
Threads that predate federation have none, and a thread that never leaves this
Server never needs one. UUIDv7 rather than v4 so the identifiers sort by
creation time and the indexes they sit in append rather than split.

### Reliability: write-before-send

The outbox row is inserted in the same transaction as the `g_*` rows it
describes. No row, no send; a row that commits is a send that happens
eventually. That is what lets a compose answer the browser before a single
packet goes to a peer, which is the rule the design puts above everything else:
a user-visible operation can only fail for a local reason, never because a peer
is down.

Delivery is at-least-once with receiver-side dedup on
`(sender, envelope_id)`. Rows are claimed with a lease (ADR 19) before
any network work, so several workers are safe. The claim query partitions by
`peer_domain`, so one dead Domain delays only the envelopes addressed to it.

Retry is exponential with full jitter, 30s base, 1h cap, 72h budget. Full
jitter rather than plain exponential so a peer coming back up is not hit by
every queued sender at the same instant. On permanent failure or an exhausted
budget the sender gets a **local bounce** in their own mailbox. Without it a
permanent failure would be silent and the sender would go on believing the
message arrived, because their Sent copy exists.

The body bytes of an outbox row are stored exactly as serialized and never
re-serialized, because JSON key order is not stable and the signature covers the
body. The signature is computed per attempt over those bytes with a fresh `Date`
and nonce, because RFC 6.2 signs both and rejects a `Date` more than five
minutes old.

### Trust

Web PKI authenticates the hostname at first contact; a pinned Ed25519 key
authenticates every envelope after that, regardless of what the CA does later.
Discovery is a fixed-URL GET at `webmail.{domain}/.well-known/webmail-federation`
with no redirects honoured and a 64 KiB read cap. The descriptor carries no
endpoint URLs at all, so there is no "peer advertised a weird URL" failure mode
to defend against.

Key rotation needs no coordination: a signature that does not verify is itself
the signal to re-fetch the descriptor and try again. That is the only forced
refresh; otherwise a descriptor is cached for 24 hours and a failed peer is not
re-probed for an hour.

The descriptor is rendered from the private key the server actually signs with,
and served by the server rather than from a static file, so the published key
and the signing key cannot drift apart.

## Consequences

### What this changes

- Mail to a federated Domain reaches it. Replies correlate back onto the same
  thread on both sides.
- An unknown recipient is only onboarded on the Domain their address belongs
  to.
- The `domain` table is both the local-Domain list and the peer cache, keyed by
  name.
- There is a second binary. Delivery to a peer has a 72-hour budget and cannot
  live in a request.
- With no `WEBMAIL_FEDERATION_CONFIG` every address is local. The disabled service answers `IsLocal` true for
  everything rather than being nil, so no caller carries a nil check.

### Choices the specification left open

**Mail arriving from outside is never relayed onward.** A message accepted from
SES has an external sender with no tenancy here and therefore no Domain to sign
as. Its local recipients are delivered; remote ones are dropped and logged.
Relaying would mean signing someone else's content as this Domain, which is a
spam and impersonation vector the protocol has no answer for.

**Every Destination is told the whole subscriber set**, not just its own share,
so the thread's membership is reproducible on each side. Each Domain delivers
only to the addresses it is authoritative for. The list is capped at 100 per
RFC 5.3, with the destination's own recipients ordered first so a truncation
loses only membership it could not act on.

**Address validation is deliberately shallow**: one `@`, a non-empty local part,
a domain containing a dot. Anything stricter starts rejecting addresses peers
legitimately hold, and the domain part is the only piece this protocol routes
on.

### Known gaps

- **No inbound rate limiting.** RFC 9.10 makes it advisory and the response
  code (`429`) is implemented on the sending side, but nothing here counts.
- **Key rotation is supported on the receiving side only.** A peer that rotates
  is followed correctly; this Domain has no way to publish a
  `previous_public_keys` set of its own, so rotating our own key is a
  flag-day operation.
- **`in_reply_to` is the thread's own identifier**, not the specific message
  being answered. That is enough for thread correlation, which is all the RFC
  uses it for, but it cannot express a branching reply.

## Configuration

`WEBMAIL_FEDERATION_CONFIG` names a JSON file. Unset disables federation.

```json
{
  "domains": [
    { "domain": "103mail.com", "customerId": 1,
      "privateKeyFile": "/etc/webmail/keys/103mail.com.ed25519.pem" }
  ],
  "allowedPeers": [],
  "maxEnvelopeBytes": 10485760,
  "maxAttachmentBytes": 26214400,
  "extraCaFile": ""
}
```

A file rather than environment variables because each Domain needs its own
keypair and a list of them does not fit a flat namespace. The Federation
hostname is **derived** from `domain` and cannot be configured; a value carrying
a scheme, a port, an IP address or a `webmail.` prefix is refused at load time,
which is where a deployment would otherwise drift from the one-hostname rule.

`extraCaFile` adds a certificate authority to the pool used for peer
connections. It exists for local and staging deployments whose peers hold
private-CA certificates. Adding a CA to our own trust store is not the same as
skipping verification, which this package never does: TLS 1.2 is the floor and
the certificate is always checked.
