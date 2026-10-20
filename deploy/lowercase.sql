-- Lowercase every stored email address (ADR 19).
--
-- PostgreSQL and SQLite compare text exactly, where MySQL's default collation
-- ignored case, so the Go backend stores and looks up addresses in lowercase.
-- Data from the MySQL database has to be brought in line once, or a login or
-- a delivery that used to match an address in another case silently stops
-- matching. OR IGNORE skips a row whose lowercase form would collide with one
-- that already exists, such as an address subscribed twice in two cases.
--
-- Written for SQLite, which the local stack runs. The cutover to PostgreSQL
-- needs the same updates, with the collisions resolved by hand.
UPDATE OR IGNORE "user" SET "username" = lower("username") WHERE "username" <> lower("username");
UPDATE OR IGNORE "alias" SET "email" = lower("email") WHERE "email" <> lower("email");
UPDATE OR IGNORE "contact" SET "email" = lower("email") WHERE "email" <> lower("email");
UPDATE OR IGNORE "g_subscription" SET "email" = lower("email") WHERE "email" <> lower("email");
UPDATE "g_message" SET "sender_email" = lower("sender_email") WHERE "sender_email" <> lower("sender_email");
UPDATE "g_chat" SET "created_by_email" = lower("created_by_email") WHERE "created_by_email" <> lower("created_by_email");
UPDATE "g_chat_message" SET "sender_email" = lower("sender_email") WHERE "sender_email" <> lower("sender_email");
UPDATE "g_chat_participant" SET "email" = lower("email") WHERE "email" <> lower("email");
UPDATE "g_attendee" SET "email" = lower("email") WHERE "email" <> lower("email");
UPDATE "g_meeting" SET "organizer_email" = lower("organizer_email") WHERE "organizer_email" <> lower("organizer_email");
UPDATE "message_status" SET "recipient_email" = lower("recipient_email") WHERE "recipient_email" <> lower("recipient_email");
