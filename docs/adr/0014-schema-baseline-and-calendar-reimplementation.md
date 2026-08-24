# 14. Schema baseline, and calendar reimplemented from the spec

Status: accepted
Date: 2026-08-24
Source: `../webmail` (OpenAPI definition, schema, CalendarClient)

## Context

Porting the API surfaced three descriptions of the calendar that disagree.

The live `webmail` database and the test fixture at
`tests/Integration/Base/GenericTestCase.sql` are identical for 31 of 35 tables.
They differ only in four: `g_attendee.will_attend` is a `tinyint` in the live
database and a three-valued enum in the fixture, `g_meeting` has no `is_all_day`
in the live database, `g_schedule` has `human_readable` rather than
`recurrence_dates`, `exception_dates` and `repeat_rule_human_readable`, and
`meeting` has no `will_attend`.

The PHP `CalendarClient` matches neither. It reads a `calendar` table that does
not exist, and writes `g_meeting` columns named `g_schedule_id`, `repeat_rule`,
`sender_email` and `sender_name` that also do not exist. Running
`GET /api/v1/calendar/calendar/{customerId}` against the live database returns
HTTP 500. The calendar code cannot have run in its current form.

The OpenAPI definition agrees with the fixture: `Meeting.isAllDay` and
`Meeting.willAttend` have nowhere to live under the live schema.

## Decision

The fixture schema is the baseline, captured as `schema/webmail.sql` with
`schema/webmail.dbml` as its readable model. Implementing the OpenAPI requires
it, so the choice follows from the API being the deliverable.
`schema/migrations/0001-calendar-rsvp-and-recurrence.sql` brings an older
database up to it and touches only those four tables.

Calendar is reimplemented from the schema and the OpenAPI rather than ported,
because there is no working implementation to port. A calendar is a mailbox:
`calendarId` equals `mailboxId`, which is what the meeting listing already
assumed by reporting `meeting.mailbox_id` as the `calendarId`. That removes the
need for the missing `calendar` table.

Recurrence expands through `internal/rrule`, a subset of RFC 5545 covering
FREQ, INTERVAL, COUNT, UNTIL, BYDAY and BYMONTHDAY. Parts outside the subset are
reported as errors rather than ignored, so a rule is never expanded into the
wrong dates.

Chat presence, typing indicators and delivery status are implemented against
`presence_status`, `typing_indicator` and `message_status`. The PHP returns
hard-coded placeholders for all three, which is why the OpenAPI marks them "501
Not yet implemented"; the tables exist, so the endpoints now use them.

## Consequences

- The calendar endpoints work, and are the one place where the Go backend is not
  bug-compatible with the PHP. Everything else was compared response by response
  against the running PHP backend and matches byte for byte.
- A deployment must run the migration before the calendar endpoints work.
  Without it they fail on the missing columns rather than degrading quietly.
- The PHP calendar code stays broken. Since ADR 2 freezes that repo, this is
  accepted rather than fixed there.
- Presence ages on read: a status nobody has refreshed for 10 minutes reads as
  away and for 15 as offline, so a client that stops reporting does not appear
  online forever.
- `../dbml-tools` cannot generate this schema. Its `tosql` emits foreign keys
  with no ON DELETE or ON UPDATE action, and this schema has 60 cascades. The
  DBML is therefore a model for review, not the generation source, which
  qualifies ADR 12 until the tool is fixed.
