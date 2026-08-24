# 5. Immutable message history

Status: accepted
Date: 2026-08-24
Source: `../webmail/specs/001-internal-messaging/spec.md` (FR-030), constitution
principle II

## Context

A conversation people rely on has to mean the same thing to everyone reading it,
including later. If a sender can delete or edit what they wrote, a reply can end
up answering a message that no longer exists, and a record that compliance
depends on can be rewritten by the person it implicates.

At the same time users do make mistakes, and refusing every correction is not
tenable either. Chat has different expectations from mail here: a typo corrected
seconds later is normal in chat and unusual in mail.

## Decision

Messages in mail threads are append-only. There is no user-facing delete or edit
of `g_message`. A user may soft-delete their own projection row, which hides the
message from their view and leaves it intact for everyone else.

Chat allows edit and delete within 15 minutes of sending, by the sender only.
This is recorded as `g_chat_message.edited_at` and `deleted_at`; the original
body is retained. A deleted chat message renders as a placeholder rather than
disappearing, so the conversation flow stays intelligible.

The only path that removes canonical content is a retention purge. Purges are
driven by admin-configured policy, run in a scheduled job, and are themselves
written to the audit log.

## Consequences

- "Delete" in the UI means different things in mail and chat, and the API has to
  make that distinction explicit rather than offering one generic delete.
- Storage only grows until retention policy is implemented. A deployment without
  a retention policy keeps everything, which is a deliberate default but needs to
  be said out loud to operators.
- Enforcement has to be server-side. A client that offers an edit for an
  18-minute-old chat message must be rejected by the backend, not merely
  discouraged.
