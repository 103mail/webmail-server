# 6. External recipient onboarding via single-use tokens

Status: accepted
Date: 2026-08-24
Source: `../webmail/specs/001-internal-messaging/spec.md` (US2, FR-004),
`002-calendaring/spec.md` (FR-005), `003-real-time-chat/spec.md` (FR-016),
constitution principle VII

## Context

Users need to message, invite and add to group chats people who have no account
yet. Since content cannot be emailed (ADR 4), the notification email cannot carry
the message, so it has to carry a way in instead. That link is the weakest point
in the system: it arrives in an inbox the system does not control, it may be
forwarded, and it may sit there for months.

The PHP repo already models this. Unknown recipients become `user` rows without a
validated login, participation is recorded in `g_subscription` by email, and a
`token` row carries the invite.

## Decision

An unknown recipient email becomes a pending user plus a `token` row. The
notification email states that a message is waiting and carries a link containing
only that opaque token.

Tokens are single-use, time-bound (`valid_until`), scoped to one user, and
non-guessable. `validated_at` and `used_at` record their lifecycle. Nothing but
the token and minimal routing metadata appears in the URL.

Until onboarding completes the recipient sees the onboarding landing page and
nothing else. Projections are created when they accept.

An expired or spent token is not an error page: the user is sent to
authentication and then to the view the link pointed at. It never reveals thread
content.

## Consequences

- Deep links in digests use the same mechanism, so there is one token model to
  get right rather than several.
- Someone who forwards the email hands over the invitation, and the token has to
  be short-lived enough that this matters less than the convenience of the link.
- Invite email that bounces has to surface to the sender, otherwise a recipient
  silently never arrives. Undeliverable invites are marked and retried with
  backoff.
- Rate limiting on invites is required, since invite email is an outbound channel
  any user can trigger against an arbitrary address.
