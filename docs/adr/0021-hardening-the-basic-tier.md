# 21. Hardening the Basic tier

Status: accepted
Date: 2026-10-21

## Context

ADR 17 left the Basic tier working but with gaps that the end-to-end stories
in `../webmail-e2e` (M1, stories 03, 06, 07, 08 and 09) ran into:

- A permanent failure bounced with free text, while RFC 6.5 asks for the
  status and the `error` code the peer answered.
- Nothing stopped an Origin from sending an attachment over the peer's
  `max_attachment_bytes`, which RFC 7 forbids, and nothing told our own people
  a file was over our limit until a peer refused it.
- A peer could write into a thread held here as anyone on its own domain, and
  make them a member, by listing them in `thread.members`.
- Rotating our own key was a flag day: there was no way to publish the old key
  as a previous one (RFC 9.5).

## Decision

- **Bounces carry the wire code.** The outbox reads the peer's `{"error": ...}`
  into the bounce, and refusals we make ourselves before sending use the same
  codes.
- **Attachment limits on both sides.** Before sending, an envelope with a file
  over the peer's cap is refused as `413 too_large` and bounced. At compose
  time, a file over our own cap is answered `413 too_large` naming the limit,
  as is a request over the body limit.
- **Only members write to a thread.** A message on a thread held here from a
  sender who is not a member of it is answered `425 not_a_member`, and
  `thread.members` never adds the sender. 425 is transient, so a member whose
  invitation is still on its way gets in on a retry; a stranger never does. A
  Domain's `mailer-daemon` is exempt. RFC 5.6 and 6.3 now say so.
- **Own-key rotation.** A Domain lists its retired keys as `previousKeys`,
  each with `retiredAt`, and the descriptor publishes each one for 48 hours
  after that by the database clock, and never after. The steps are in
  `docs/runbooks/key-rotation.md`.

## Consequences

- ADR 17's gap "key rotation is supported on the receiving side only" is
  closed, and so is the one about inbound rate limiting: a peer is answered
  `429` with `Retry-After` past its limit per minute or per day (RFC 9.10).
  `in_reply_to` naming the thread is now intended (RFC 5.5).
- A bounce's `Reason:` line is a wire code, which the end-to-end stories
  compare exactly.
- The local stack gains what the stories need to show these: a scripted peer,
  `peer.test`; a fault proxy in front of every inbound endpoint; per-story
  federation config overrides; and read-only controls for pinned peers, stored
  files and thread members (ADR 20). See `deploy/README.md`.
