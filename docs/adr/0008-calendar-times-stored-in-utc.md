# 8. Calendar times stored in UTC with a separate IANA zone

Status: accepted
Date: 2026-08-24
Source: `../webmail/specs/002-calendaring/spec.md` (US1, US5, US6, FR-003,
FR-004)

## Context

An event created in Amsterdam and read in New York has to show the right local
time in both places, and has to keep meaning the same thing across a daylight
saving transition. Storing local time with an offset breaks on DST, because the
offset is a property of an instant and not of a zone. Storing only UTC loses the
information needed to expand a recurrence correctly, since "every Tuesday at
09:00 local" is not a fixed UTC interval across a DST boundary.

All-day events are a third case: they have a date but no meaningful instant, and
forcing them through a timestamp makes them shift across zones.

## Decision

`start_time` and `end_time` are stored in UTC. `time_zone` stores the IANA
identifier (`Europe/Amsterdam`) and is used for display and for recurrence
expansion, never as a stored offset.

All-day events set `is_all_day` and store times at midnight in the event's
timezone. They render as a date with no time.

Recurrence lives in `g_schedule.repeat_rule` as an RRULE, with
`recurrence_dates` for irregular extra occurrences and `exception_dates` for
exclusions. `repeat_rule_human_readable` carries the description for display.
Editing a single occurrence generates a concrete `g_meeting` for it and leaves
the series intact.

## Consequences

- Recurrence expansion has to load the zone database, so the deployment needs
  current tzdata and the Go binary must not be built without it.
- A zone whose rules change after an event is created will move that event's UTC
  instant on the next expansion. That is the correct behaviour for "every Tuesday
  at 09:00" and needs to be understood rather than patched around.
- Storing both a UTC instant and the zone means they can disagree. The UTC value
  is authoritative for ordering and reminders; the zone is authoritative for
  what the user meant.
- The PHP repo uses `rlanvin/php-rrule`. The Go implementation needs an RRULE
  library with matching semantics, and the two must agree while both backends
  run.
