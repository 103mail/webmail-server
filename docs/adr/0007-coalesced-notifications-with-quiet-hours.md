# 7. Coalesced notifications with quiet hours

Status: accepted
Date: 2026-08-24
Source: `../webmail/specs/001-internal-messaging/spec.md` (US4, FR-007, FR-018),
`002-calendaring/spec.md` (FR-006), `003-real-time-chat/spec.md` (FR-014),
constitution principle VI

## Context

A busy thread produces a burst of activity, and one email per event turns into a
notification storm that users answer by muting everything. Email is also the
channel that cannot carry content (ADR 4), so per-event email would be a stream
of near-identical "something happened" messages, which is the least useful form
it could take.

In-app notification has the opposite property: it is cheap, immediate, and can
show content to someone already authenticated.

## Decision

In-app notification is immediate for all three features. Chat targets delivery
within 2 seconds, calendar changes within 5 seconds.

Email is a digest only. `notification_preference.frequency` is one of
`hourly_digest`, `daily_digest` or `none`, defaulting to hourly. A digest is sent
only when unread items exist, summarises unread threads by folder, contains no
message content, and carries a deep link to the mailbox filtered to unread. At
most one daily digest per calendar day per user.

`quiet_hours_start` and `quiet_hours_end` queue notifications rather than dropping
them; queued notifications are delivered when quiet hours end.

Chat adds per-conversation overrides in `chat.notification_preference` (`all`,
`mentions_only`, `none`) which take precedence over the global setting. An
`@mention` notifies even a muted conversation.

Calendar reminders are separate from digests: `reminder_event` carries
`scheduled_at` and `sent_at`, and `sent_at` is what prevents duplicate delivery.

## Consequences

- Digest generation is a scheduled job, and it must be idempotent, since a job
  that runs twice must not send two digests.
- A digest with nothing unread is skipped rather than sent empty, and the skip is
  logged so an operator can tell "no digest" from "digest failed".
- Mention parsing becomes notification-critical, not cosmetic. A mention that
  fails to resolve is silently a missed notification, so unresolved mentions are
  treated as plain text and never as a delivery target.
- Preference resolution has a fixed precedence: mention, then per-conversation,
  then global frequency, then quiet hours. Implement it in one place.
