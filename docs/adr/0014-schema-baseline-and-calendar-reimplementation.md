# 14. Schema baseline, and calendar reimplemented from the spec

Status: accepted
Date: 2026-08-24
Source: `../webmail` (OpenAPI definition, schema, CalendarClient)

## Context

Porting the API surfaced three descriptions of the calendar that disagree.

The live `webmail` database and the test fixture at
`tests/Integration/Base/GenericTestCase.sql` differ in four of 35 tables:
`g_attendee.will_attend` is a `tinyint` live and a three-valued enum in the
fixture, `g_meeting` has no `is_all_day` live, `g_schedule` has `human_readable`
rather than `recurrence_dates`, `exception_dates` and
`repeat_rule_human_readable`, and `meeting` has no `will_attend`.

The PHP `CalendarClient` matches neither. It reads a `calendar` table and
`g_meeting` columns that do not exist, and returns HTTP 500 against the live
database. The OpenAPI definition agrees with the fixture.

## Decision

The fixture schema is the baseline, captured in `schema/webmail.dbml`, because
implementing the OpenAPI requires it. An older database is brought up to it with `dbml-tools migrate` (ADR 12).

Calendar is reimplemented from the schema and the OpenAPI rather than ported,
because there is no working implementation to port. A calendar is a mailbox:
`calendarId` equals `mailboxId`, which removes the need for a `calendar` table.

Recurrence expands through `internal/rrule`, a subset of RFC 5545 covering
FREQ, INTERVAL, COUNT, UNTIL, BYDAY and BYMONTHDAY. Parts outside the subset are
reported as errors rather than ignored, so a rule is never expanded into the
wrong dates.

Chat presence, typing indicators and delivery status are implemented against
`presence_status`, `typing_indicator` and `message_status`, where the PHP returns
hard-coded placeholders.

## Consequences

- The calendar endpoints are the one place where the Go backend is not
  bug-compatible with the PHP. Everything else was compared response by response
  against the running PHP backend and matches byte for byte.
- A deployment must be migrated before the calendar endpoints work. Without it
  they fail on the missing columns rather than degrading quietly.
- The PHP calendar code stays broken, since ADR 2 freezes that repo.
- Presence ages on read: a status nobody has refreshed for 10 minutes reads as
  away and for 15 as offline.
