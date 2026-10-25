# 3. Canonical objects with per-user projections

Status: accepted
Date: 2026-08-24
Source: `../webmail/specs/001-internal-messaging/data-model.md`

## Context

A thread is shared by several people, but read state, folder placement and
deletion are personal. Storing those on the shared row would make one user's
actions visible to everyone. Storing whole copies per user would let the copies
drift and would make "this message cannot be deleted" unenforceable, because each
user would own their own copy of the content.

Access control has the same problem from the other side. If visibility is
computed from an ACL evaluated at read time, every query needs the ACL logic
repeated, and a mistake in one query silently leaks content.

## Decision

Split every shared object into a canonical row and per-user projection rows.

Canonical tables are prefixed `g_` and hold the content: `g_thread`, `g_message`,
`g_attachment`, `g_subscription`, `g_chat`, `g_chat_message`,
`g_chat_attachment`, `g_chat_participant`, `g_meeting`, `g_meeting_revision`,
`g_meeting_attendee`, `g_meeting_rsvp`, `g_meeting_document`. They are
immutable to end users. A meeting changes by stacking revisions, not by
editing rows (ADR 22).

Projection tables hold per-user state and are keyed by `customer_id`: `thread`,
`message`, `attachment`, `chat`, `chat_message`, `meeting`. They carry
`read_at`, `deleted_at`, `seen_revision`, folder placement and notification
preference. An answer to a meeting is a `g_meeting_rsvp` row, not projection
state, because every attendee sees every answer (RFC 15.5).

Visibility follows from the existence of a projection row, plus membership in
`g_subscription` or `g_chat_participant` by email. There is no separate ACL.
`read_at` and `deleted_at` are never written to a `g_` table.

## Consequences

- A user deleting a thread removes it from their view only. Other participants
  are unaffected, and the canonical content stays for audit and retention.
- Access control is a join, not a rule engine. A query that forgets the
  projection returns nothing rather than returning someone else's mail.
- Fan-out on send is real work: one canonical write plus a projection row per
  recipient per message and attachment. This is the cost that buys the other two
  properties, and it is why delivery runs through workers rather than inline.
- Search must run against canonical fulltext indexes and then filter by
  accessible projections, never the other way around.
