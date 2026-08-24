# 11. Federation over HTTPS, not SMTP

Status: accepted
Date: 2026-08-24
Source: `../webmail-federation/project_rfc.md` (v0.2 draft),
`project_design.md`

Note: this decision comes from the federation repo, not from the PHP feature
specs. It is recorded here because it constrains the backend directly.

## Context

The PHP backend assumes one domain. `EmailClient::getDefaultDomain()` treats the
`domain` table as a singleton, and an unknown recipient is auto-onboarded with a
generated alias on that domain. Under multiple deployments this is wrong: a
recipient at another participating domain would be given a local alias instead of
being delivered to.

Falling back to SMTP between deployments would give up what the system is for.
SMTP carries content to servers with no shared rules, which contradicts ADR 4.

## Decision

Domains exchange messages over HTTPS using the Webmail-Federation protocol.
Discovery is `GET /.well-known/webmail-federation`, cached up to 24 hours.
Transfer is a single signed `POST /api/v1/federation/inbound` carrying the
envelope and all attachment bytes, answered with `202 Accepted`. There is no
separate fetch step.

The reliability rules from the RFC are binding on this backend:

- User-facing actions never block on a remote peer. Intent is persisted locally
  and acknowledged, then a background worker performs the remote call.
- Mutating requests carry a UUIDv7 chosen before the first attempt and reused on
  every retry. Receivers deduplicate on `(X-Federation-Sender, identifier)` and
  replay the original terminal response without re-applying side effects.
- Read requests may be synchronous but always carry finite connect and read
  timeouts, with a specified fallback on failure.
- No synchronous fan-out. Each destination is dispatched independently so one
  slow peer cannot delay the others.
- An exhausted retry budget or a terminal 4xx produces a local bounce. A delivery
  that silently disappears is not conformant.

Recipient resolution splits into local and remote branches. The auto-onboarding
shortcut for unknown addresses applies only to the local branch.

## Consequences

- The `domain` table stops being a singleton, and every place that calls
  `getDefaultDomain()` in the PHP code becomes a decision point in the Go port.
- A durable outbound queue with retry state is required before federation can
  ship. It is the same worker infrastructure digests and reminders need (ADR 7).
- Signature verification and peer descriptor caching are on the critical path of
  every inbound message, so their failure modes need to be explicit.
- The RFC is a draft below version 1.0, so wire format and endpoint paths may
  still change incompatibly. Pin the protocol version in code and treat it as a
  moving target until 1.0.
