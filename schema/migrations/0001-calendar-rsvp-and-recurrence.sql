-- Bring a database created before the calendaring work up to schema/webmail.sql.
--
-- Only the four calendar tables differ. Everything else already matches.
--
-- The columns added here are required by the OpenAPI definition: Meeting.isAllDay
-- and Meeting.willAttend have nowhere to live without them, and the recurrence
-- columns hold what g_schedule.repeat_rule expands to.
--
-- Review before running. `g_attendee.will_attend` changes type, and the mapping
-- below assumes the old tinyint meant 1 = yes, 0 = no, NULL = no answer yet.

START TRANSACTION;

-- g_attendee: an RSVP is three-valued, not a boolean, and each response is
-- timestamped so the row sequence is an audit trail.
ALTER TABLE `g_attendee`
  ADD COLUMN `will_attend_new` enum('yes','no','maybe') NOT NULL DEFAULT 'maybe' AFTER `will_attend`,
  ADD COLUMN `created_at` datetime NOT NULL DEFAULT current_timestamp();

UPDATE `g_attendee` SET `will_attend_new` = CASE
  WHEN `will_attend` = 1 THEN 'yes'
  WHEN `will_attend` = 0 THEN 'no'
  ELSE 'maybe'
END;

ALTER TABLE `g_attendee` DROP COLUMN `will_attend`;
ALTER TABLE `g_attendee` CHANGE `will_attend_new` `will_attend` enum('yes','no','maybe') NOT NULL DEFAULT 'maybe';

-- g_meeting: all-day events carry a flag rather than being inferred from times.
ALTER TABLE `g_meeting`
  ADD COLUMN `is_all_day` tinyint(1) NOT NULL DEFAULT 0 AFTER `time_zone`;

-- g_schedule: explicit extra and excluded occurrence dates, and a rendering
-- named after the rule it describes.
ALTER TABLE `g_schedule`
  ADD COLUMN `recurrence_dates` mediumtext NOT NULL AFTER `repeat_rule`,
  ADD COLUMN `exception_dates` mediumtext NOT NULL AFTER `recurrence_dates`,
  CHANGE `human_readable` `repeat_rule_human_readable` varchar(255) NOT NULL;

-- meeting: the attendee's effective (last) RSVP, so a listing does not have to
-- aggregate g_attendee.
ALTER TABLE `meeting`
  ADD COLUMN `will_attend` enum('yes','no','maybe') DEFAULT NULL AFTER `schedule_id`;

COMMIT;
