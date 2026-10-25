# 22. Meetings as a stack of revisions

Status: accepted
Date: 2026-10-25
Source: `../webmail-federation/project_rfc.md` (§15)

## Context

A meeting changes after it is sent: it is moved, renamed, cancelled, given
documents and new attendees, and attendees answer and change their answer. The
rest of the system never changes what it has sent. A message is append-only
(ADR 5), and canonical rows are immutable to end users (ADR 3).

Today the calendar does not live up to that. `g_meeting` holds the content, a
series is expanded into up to 100 `g_meeting` rows, and editing one occurrence
makes another concrete row (ADR 8). Deleting a meeting removes the user's
`meeting` row. There is no way to change a meeting at all, and nothing a
federated Domain could apply a change to.

RFC §15 sends a meeting between Domains as revisions: the whole meeting, with a
number that only goes up, sent only by the organizer's Domain. Answers travel
separately, each from the attendee's own Domain.

## Decision

A meeting is stored the way a thread is. Every change is a new revision stacked
on the ones before, like a message in a thread, and every answer is a new
answer stacked on the attendee's earlier ones. No canonical row is ever updated
or deleted. What a user sees is the latest revision and their latest answer,
and earlier ones are the meeting's history. Hiding and deleting happen only in
the user's own projection.

The canonical tables, each prefixed with `g_meeting_` as the chat tables are
with `g_chat_`:

- **`g_meeting`** is the meeting, as `g_thread` is the thread:
  `federation_id`, `organizer_email`, `organizer_name`, `created_at`. It holds
  no content.
- **`g_meeting_revision`** is one revision, as `g_message` is one message:
  `g_meeting_id`, `revision` (unique per meeting), `status`, `title`,
  `description`, `description_html`, `start_time`, `end_time`, `time_zone`,
  `is_all_day`, `repeat` and `overrides` as the JSON of RFC §15.6, `changed_at`
  (when the organizer made the change) and `created_at` (when it was stored
  here). `first_start` and `last_end`, the latter null for a series without
  end, let a calendar query find candidate meetings before expanding them.
- **`g_meeting_attendee`** lists who a revision invites:
  `g_meeting_revision_id`, `email`. A revision lists all its attendees, so who is invited is read from
  the latest revision alone.
- **`g_meeting_rsvp`** is one answer: `g_meeting_id`, `email`, `will_attend`,
  `revision` (the revision answered), `occurrence` (null for the whole
  meeting), `answered_at`, `created_at`. This is the audit trail `g_attendee`
  holds today, moved to a table of its own.
- **`g_meeting_document`** belongs to a revision (`g_meeting_revision_id`),
  since each revision carries all of the meeting's documents. The bytes are
  stored once per hash, so a document repeated in later revisions costs a row,
  not a copy.

`g_schedule` and the `schedule` projection go. A series is a property of a
revision, and occurrences are expanded from `repeat` and `overrides` when a
calendar is read, not stored as rows. A single occurrence is changed or
cancelled by a new revision with an override, never by a row of its own.

The projection `meeting` is one row per customer per meeting, not per
occurrence: `customer_id`, `g_meeting_id`, `mailbox_id`, `seen_revision` and
`deleted_at`. `seen_revision` does for a meeting what `read_at` does for a
message, so a change can be shown as new until the user has looked at it.
`will_attend` leaves the projection: a user's answer is their latest
`g_meeting_rsvp`.

The rules:

- **Revisions are ordered by number.** Only the organizer's Domain makes them,
  so the number is a total order. A revision that arrives late is stored in its
  place and does not change what is shown unless it is the highest.
- **Answers are ordered by `answered_at`.** A user's answer for an occurrence
  is the later of their answer for the whole meeting and for that occurrence
  (RFC §15.5). An older answer arriving late is stored and changes nothing.
- **Only the organizer changes a meeting.** Locally and over federation alike,
  a new revision can only be written for the organizer, and attendees are
  added and removed only by the organizer. `AddAttendee` loses its current
  rule that any attendee can invite.
- **Deleting is personal.** Deleting a meeting sets `deleted_at` on the user's
  projection. A new revision brings it back, as a new message brings back a
  deleted chat. The user is still an attendee and their answer stands; to tell
  the others they are not coming, they answer `no`.
- **Removed attendees keep what they had.** A revision that removes someone is
  delivered to them too, and their projection stays, showing the meeting as
  cancelled for them. They receive no later revisions.
- **History starts at the invitation.** A newly invited attendee's Domain gets
  the current revision only, not the earlier ones, so their history begins
  where they came in. Earlier answers do reach them, from each attendee's own
  Domain (RFC §15.4).

Local meetings use the same tables and rules. A meeting with no attendee on
another Domain is a meeting whose revisions happen not to leave the Server.

## Consequences

- The calendar keeps the same guarantee as mail and chat: nobody can rewrite
  what others were shown. A meeting's history can show who moved it and when,
  by comparing a revision with the one before.
- Changing a meeting becomes possible at all, which the current API cannot do.
  It needs an update route that writes a revision.
- Reading a calendar expands rules at read time. The `first_start` and
  `last_end` columns keep that to the meetings that can fall in the range, but a
  calendar with many open-ended series costs more to read than one with stored
  occurrences.
- A meeting with large documents that changes often grows by a row per
  document per revision, which is the price of every revision being
  self-contained, as RFC §7 asks of every envelope.
- `internal/rrule` reads the JSON of RFC §15.6 instead of an RRULE string, and
  `repeat_rule_human_readable` is computed rather than stored.
- Existing data migrates once: each `g_schedule` with its `g_meeting` rows
  becomes one `g_meeting` with revision 1, an occurrence that was edited on its
  own becomes an override, each `g_attendee` row becomes a `g_meeting_attendee`
  and, with its answer, a `g_meeting_rsvp`, and `g_document` becomes
  `g_meeting_document`. This belongs to the M2 migration rehearsal.
- ADR 3 (canonical table list), ADR 8 (recurrence and single occurrences) and
  ADR 14 (RRULE subset) are edited to match.

## As built

- `g_meeting_document` keeps `hash_md5`, the key of the blob store, and adds
  `federation_id` and `hash_sha256`, which the envelope names a document by.
- Revisions arrive with no time of change, so for a revision received from
  another Domain `changed_at` is when it was stored here.
- A removed attendee is shown the first revision held after the last one that
  listed them: the one that removed them, or, when that one never arrived, the
  first that told their Domain they were out (RFC 15.4).
- Inviting someone on another Domain waits up to five seconds on its
  descriptor, in the request, because RFC 15.1 asks for the organizer to be
  told at once. A Domain that cannot be discovered then is treated as no
  Webmail Domain, and the address is not invited.
- A queued revision or answer is marked `superseded` in the outbox when a
  later one for the same peer replaces it, which is also how a backfill sends
  only the latest revision (RFC 15.3).
