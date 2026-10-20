-- Generated from webmail.dbml by generate.sh. Edit that, not this.

CREATE TYPE "will_attend" AS ENUM (
    'yes',
    'no',
    'maybe'
);

CREATE TYPE "audit_log_operation" AS ENUM (
    'insert',
    'update',
    'delete'
);

CREATE TYPE "chat_notification_preference" AS ENUM (
    'all',
    'mentions_only',
    'none'
);

CREATE TYPE "g_chat_type" AS ENUM (
    'direct',
    'group'
);

CREATE TYPE "g_chat_participant_role" AS ENUM (
    'creator',
    'admin',
    'member'
);

CREATE TYPE "message_status_status" AS ENUM (
    'sent',
    'delivered',
    'read'
);

CREATE TYPE "notification_preference_frequency" AS ENUM (
    'daily_digest',
    'hourly_digest'
);

CREATE TYPE "presence_status_status" AS ENUM (
    'online',
    'offline',
    'away'
);

CREATE TABLE "alias" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "email" varchar(255) NOT NULL UNIQUE,
    "name" varchar(255) NOT NULL,
    "mailbox_id" bigint NOT NULL
);
CREATE INDEX "alias_customer_id" ON "alias" ("customer_id");
CREATE INDEX "alias_mailbox_id" ON "alias" ("mailbox_id");

CREATE TABLE "attachment" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "message_id" bigint NOT NULL,
    "g_attachment_id" bigint NOT NULL,
    "read_at" timestamp,
    "deleted_at" timestamp
);
CREATE INDEX "attachment_customer_id" ON "attachment" ("customer_id");
CREATE INDEX "g_attachment_id" ON "attachment" ("g_attachment_id");
CREATE INDEX "message_id" ON "attachment" ("message_id");

CREATE TABLE "audit_log" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint,
    "user_id" bigint,
    "username" varchar(255) NOT NULL,
    "is_admin" boolean NOT NULL DEFAULT FALSE,
    "created_at" timestamp NOT NULL,
    "operation" "audit_log_operation" NOT NULL,
    "entity" varchar(255) NOT NULL,
    "identifier" varchar(255) NOT NULL,
    "changes_before" text NOT NULL,
    "changes_after" text NOT NULL
);
CREATE INDEX "created_at" ON "audit_log" ("created_at");
CREATE INDEX "audit_log_customer_id" ON "audit_log" ("customer_id");
CREATE INDEX "entity" ON "audit_log" ("entity");
CREATE INDEX "audit_log_user_id" ON "audit_log" ("user_id");

CREATE TABLE "chat" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "mailbox_id" bigint NOT NULL,
    "g_chat_id" bigint NOT NULL,
    "last_read_message_id" bigint,
    "muted" boolean NOT NULL DEFAULT FALSE,
    "notification_preference" "chat_notification_preference" NOT NULL DEFAULT 'all',
    "deleted_at" timestamp
);
CREATE INDEX "chat_customer_id" ON "chat" ("customer_id");
CREATE INDEX "chat_g_chat_id" ON "chat" ("g_chat_id");
CREATE INDEX "last_read_message_id" ON "chat" ("last_read_message_id");
CREATE INDEX "chat_mailbox_id" ON "chat" ("mailbox_id");

CREATE TABLE "chat_session" (
    "id" BIGSERIAL PRIMARY KEY,
    "client_id" varchar(64) NOT NULL UNIQUE,
    "customer_id" bigint NOT NULL,
    "user_id" bigint NOT NULL,
    "created_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "expires_at" timestamp NOT NULL,
    "connected_at" timestamp,
    "last_seen_at" timestamp,
    "closed_at" timestamp
);
CREATE INDEX "chat_session_user_id" ON "chat_session" ("user_id");
CREATE INDEX "chat_session_customer_id" ON "chat_session" ("customer_id");
CREATE INDEX "closed_at" ON "chat_session" ("closed_at");

CREATE TABLE "chat_message" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "chat_id" bigint NOT NULL,
    "g_chat_message_id" bigint NOT NULL,
    "delivered_at" timestamp,
    "read_at" timestamp
);
CREATE INDEX "chat_id" ON "chat_message" ("chat_id");
CREATE INDEX "chat_message_customer_id" ON "chat_message" ("customer_id");
CREATE INDEX "chat_message_g_chat_message_id" ON "chat_message" ("g_chat_message_id");

CREATE TABLE "contact" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "email" varchar(255) NOT NULL,
    "name" varchar(255) NOT NULL,
    "created_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "deleted_at" timestamp
);
CREATE UNIQUE INDEX "customer_id_email" ON "contact" ("customer_id", "email");

CREATE TABLE "customer" (
    "id" BIGSERIAL PRIMARY KEY,
    "name" varchar(255) NOT NULL,
    "default_mailbox_id" bigint
);
CREATE INDEX "default_mailbox_id" ON "customer" ("default_mailbox_id");

CREATE TABLE "domain" (
    "id" SERIAL PRIMARY KEY,
    "name" varchar(255) NOT NULL UNIQUE,
    "customer_id" bigint,
    "is_local" boolean NOT NULL DEFAULT FALSE,
    "public_key" varchar(255) NOT NULL DEFAULT '',
    "previous_public_keys" varchar(1024) NOT NULL DEFAULT '',
    "versions" varchar(255) NOT NULL DEFAULT '',
    "is_registrar" boolean NOT NULL DEFAULT FALSE,
    "max_envelope_bytes" bigint NOT NULL DEFAULT 10485760,
    "max_attachment_bytes" bigint NOT NULL DEFAULT 26214400,
    "discovered_at" timestamp,
    "discovery_failed_at" timestamp
);
CREATE INDEX "domain_customer_id" ON "domain" ("customer_id");
CREATE INDEX "is_local" ON "domain" ("is_local");

CREATE TABLE "email_template" (
    "id" SERIAL PRIMARY KEY,
    "name" varchar(255) NOT NULL,
    "language" varchar(2) NOT NULL,
    "sender_name" varchar(255) NOT NULL,
    "sender_email" varchar(255) NOT NULL,
    "subject" varchar(255) NOT NULL,
    "body" text NOT NULL,
    "parameters" text NOT NULL
);
CREATE UNIQUE INDEX "language_name" ON "email_template" ("language", "name");

CREATE TABLE "federation_outbox" (
    "id" BIGSERIAL PRIMARY KEY,
    "kind" varchar(32) NOT NULL DEFAULT 'envelope',
    "peer_domain" varchar(255) NOT NULL,
    "origin_domain" varchar(255) NOT NULL,
    "body" text NOT NULL,
    "idempotency_key" varchar(36) NOT NULL,
    "related_id" bigint,
    "status" varchar(20) NOT NULL DEFAULT 'pending',
    "claimed_by" varchar(64),
    "claimed_until" timestamp,
    "attempts" integer NOT NULL DEFAULT 0,
    "last_error" text,
    "last_http_status" integer,
    "next_retry_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "created_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "delivered_at" timestamp
);
CREATE UNIQUE INDEX "origin_idempotency" ON "federation_outbox" ("origin_domain", "idempotency_key");
CREATE INDEX "claimable" ON "federation_outbox" ("status", "next_retry_at");
CREATE INDEX "peer_domain" ON "federation_outbox" ("peer_domain");

CREATE TABLE "federation_inbox" (
    "id" BIGSERIAL PRIMARY KEY,
    "sender_domain" varchar(255) NOT NULL,
    "envelope_id" varchar(36) NOT NULL,
    "received_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX "sender_envelope" ON "federation_inbox" ("sender_domain", "envelope_id");
CREATE INDEX "federation_inbox_received_at" ON "federation_inbox" ("received_at");

CREATE TABLE "federation_nonce" (
    "id" BIGSERIAL PRIMARY KEY,
    "sender_domain" varchar(255) NOT NULL,
    "endpoint" varchar(32) NOT NULL DEFAULT 'inbound',
    "nonce" varchar(64) NOT NULL,
    "seen_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX "sender_endpoint_nonce" ON "federation_nonce" ("sender_domain", "endpoint", "nonce");
CREATE INDEX "seen_at" ON "federation_nonce" ("seen_at");

CREATE TABLE "folder" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "mailbox_id" bigint NOT NULL,
    "name" varchar(255) NOT NULL,
    "created_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "deleted_at" timestamp
);
CREATE INDEX "folder_customer_id" ON "folder" ("customer_id");
CREATE INDEX "folder_mailbox_id" ON "folder" ("mailbox_id");

CREATE TABLE "g_attachment" (
    "id" BIGSERIAL PRIMARY KEY,
    "g_message_id" bigint NOT NULL,
    "file_name" varchar(255) NOT NULL,
    "mime_type" varchar(255) NOT NULL,
    "file_size" integer NOT NULL,
    "hash_md5" varchar(255) NOT NULL,
    "federation_id" varchar(36) UNIQUE
);
CREATE INDEX "g_attachment_thread_id" ON "g_attachment" ("g_message_id");
CREATE INDEX "g_attachment_federation_id" ON "g_attachment" ("federation_id");

CREATE TABLE "g_attendee" (
    "id" BIGSERIAL PRIMARY KEY,
    "g_meeting_id" bigint NOT NULL,
    "email" varchar(255) NOT NULL,
    "will_attend" "will_attend" NOT NULL DEFAULT 'maybe',
    "created_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX "g_attendee_g_meeting_id" ON "g_attendee" ("g_meeting_id");

CREATE TABLE "g_chat" (
    "id" BIGSERIAL PRIMARY KEY,
    "type" "g_chat_type" NOT NULL,
    "name" varchar(255),
    "created_by_email" varchar(255) NOT NULL,
    "created_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE "g_chat_attachment" (
    "id" BIGSERIAL PRIMARY KEY,
    "g_chat_message_id" bigint NOT NULL,
    "file_name" varchar(255) NOT NULL,
    "mime_type" varchar(255) NOT NULL,
    "file_size" integer NOT NULL,
    "hash_md5" varchar(255) NOT NULL
);
CREATE INDEX "g_chat_attachment_g_chat_message_id" ON "g_chat_attachment" ("g_chat_message_id");

CREATE TABLE "g_chat_message" (
    "id" BIGSERIAL PRIMARY KEY,
    "g_chat_id" bigint NOT NULL,
    "sender_email" varchar(255) NOT NULL,
    "sender_name" varchar(255) NOT NULL,
    "body" text NOT NULL,
    "body_html" text,
    "created_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "edited_at" timestamp,
    "deleted_at" timestamp
);
CREATE INDEX "g_chat_message_g_chat_id" ON "g_chat_message" ("g_chat_id");

CREATE TABLE "g_chat_participant" (
    "id" BIGSERIAL PRIMARY KEY,
    "g_chat_id" bigint NOT NULL,
    "email" varchar(255) NOT NULL,
    "role" "g_chat_participant_role" NOT NULL,
    "joined_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "left_at" timestamp
);
CREATE INDEX "g_chat_participant_g_chat_id" ON "g_chat_participant" ("g_chat_id");

CREATE TABLE "g_document" (
    "id" BIGSERIAL PRIMARY KEY,
    "g_meeting_id" bigint NOT NULL,
    "file_name" varchar(255) NOT NULL,
    "mime_type" varchar(255) NOT NULL,
    "file_size" integer NOT NULL,
    "hash_md5" varchar(255) NOT NULL
);
CREATE INDEX "g_document_g_meeting_id" ON "g_document" ("g_meeting_id");

CREATE TABLE "g_meeting" (
    "id" BIGSERIAL PRIMARY KEY,
    "g_schedule" bigint,
    "title" varchar(255) NOT NULL,
    "start_time" timestamp NOT NULL,
    "end_time" timestamp NOT NULL,
    "time_zone" varchar(255) NOT NULL,
    "is_all_day" boolean NOT NULL DEFAULT FALSE,
    "organizer_email" varchar(255) NOT NULL,
    "organizer_name" varchar(255) NOT NULL,
    "description" text NOT NULL,
    "description_html" text,
    "created_at" timestamp DEFAULT CURRENT_TIMESTAMP,
    "sent_at" timestamp
);
CREATE INDEX "g_meeting_g_schedule" ON "g_meeting" ("g_schedule");

CREATE TABLE "g_message" (
    "id" BIGSERIAL PRIMARY KEY,
    "g_thread_id" bigint NOT NULL,
    "sender_email" varchar(255) NOT NULL,
    "sender_name" varchar(255) NOT NULL,
    "body" text NOT NULL,
    "body_html" text,
    "created_at" timestamp DEFAULT CURRENT_TIMESTAMP,
    "sent_at" timestamp,
    "federation_id" varchar(36) UNIQUE
);
CREATE INDEX "g_message_g_thread_id" ON "g_message" ("g_thread_id");
CREATE INDEX "g_message_federation_id" ON "g_message" ("federation_id");

CREATE TABLE "g_schedule" (
    "id" BIGSERIAL PRIMARY KEY,
    "start_time" timestamp NOT NULL,
    "end_time" timestamp NOT NULL,
    "time_zone" varchar(255) NOT NULL,
    "repeat_rule" text NOT NULL,
    "recurrence_dates" text NOT NULL,
    "exception_dates" text NOT NULL,
    "repeat_rule_human_readable" varchar(255) NOT NULL,
    "created_at" timestamp DEFAULT CURRENT_TIMESTAMP,
    "sent_at" timestamp
);

CREATE TABLE "g_subscription" (
    "id" BIGSERIAL PRIMARY KEY,
    "g_thread_id" bigint NOT NULL,
    "email" varchar(255) NOT NULL,
    "refused_at" timestamp,
    "left_at" timestamp
);
CREATE INDEX "g_subscription_g_thread_id" ON "g_subscription" ("g_thread_id");
CREATE UNIQUE INDEX "thread_email" ON "g_subscription" ("g_thread_id", "email");

CREATE TABLE "g_thread" (
    "id" BIGSERIAL PRIMARY KEY,
    "subject_line" varchar(255) NOT NULL,
    "body_summary" varchar(255) NOT NULL DEFAULT '',
    "created_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updated_at" timestamp DEFAULT CURRENT_TIMESTAMP,
    "federation_id" varchar(36) UNIQUE
);
CREATE INDEX "g_thread_federation_id" ON "g_thread" ("federation_id");

CREATE TABLE "inbound_email" (
    "provider_id" varchar(255) PRIMARY KEY,
    "received_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX "inbound_email_received_at" ON "inbound_email" ("received_at");

CREATE TABLE "mailbox" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "user_id" bigint,
    "name" varchar(255) NOT NULL,
    "color" varchar(255) NOT NULL,
    "default_folder_id" bigint,
    "default_alias_id" bigint,
    "created_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "deleted_at" timestamp
);
CREATE INDEX "mailbox_customer_id" ON "mailbox" ("customer_id");
CREATE INDEX "default_alias_id" ON "mailbox" ("default_alias_id");
CREATE INDEX "inbox_folder_id" ON "mailbox" ("default_folder_id");
CREATE INDEX "mailbox_user_id" ON "mailbox" ("user_id");

CREATE TABLE "meeting" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "g_meeting_id" bigint NOT NULL,
    "mailbox_id" bigint NOT NULL,
    "schedule_id" bigint,
    "will_attend" "will_attend"
);
CREATE INDEX "meeting_customer_id" ON "meeting" ("customer_id");
CREATE INDEX "meeting_g_meeting_id" ON "meeting" ("g_meeting_id");
CREATE INDEX "meeting_mailbox_id" ON "meeting" ("mailbox_id");
CREATE INDEX "schedule_id" ON "meeting" ("schedule_id");

CREATE TABLE "message" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "thread_id" bigint NOT NULL,
    "g_message_id" bigint NOT NULL,
    "read_at" timestamp,
    "deleted_at" timestamp
);
CREATE INDEX "message_customer_id" ON "message" ("customer_id");
CREATE INDEX "g_message_id" ON "message" ("g_message_id");
CREATE INDEX "message_thread_id" ON "message" ("thread_id");

CREATE TABLE "message_status" (
    "id" BIGSERIAL PRIMARY KEY,
    "g_chat_message_id" bigint NOT NULL,
    "recipient_email" varchar(255) NOT NULL,
    "status" "message_status_status" NOT NULL,
    "timestamp" timestamp NOT NULL
);
CREATE INDEX "message_status_g_chat_message_id" ON "message_status" ("g_chat_message_id");

CREATE TABLE "notification_preference" (
    "id" BIGSERIAL PRIMARY KEY,
    "user_id" bigint NOT NULL UNIQUE,
    "frequency" "notification_preference_frequency" NOT NULL DEFAULT 'daily_digest',
    "quiet_hours_start" time,
    "quiet_hours_end" time
);

CREATE TABLE "presence_status" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "user_id" bigint NOT NULL UNIQUE,
    "status" "presence_status_status" NOT NULL,
    "last_activity_at" timestamp NOT NULL,
    "updated_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX "presence_status_customer_id" ON "presence_status" ("customer_id");

CREATE TABLE "reminder_event" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "mailbox_id" bigint NOT NULL,
    "g_meeting_id" bigint NOT NULL,
    "scheduled_at" timestamp NOT NULL,
    "sent_at" timestamp
);
CREATE INDEX "reminder_event_customer_id" ON "reminder_event" ("customer_id");
CREATE INDEX "reminder_event_g_meeting_id" ON "reminder_event" ("g_meeting_id");
CREATE INDEX "reminder_event_mailbox_id" ON "reminder_event" ("mailbox_id");
CREATE INDEX "scheduled_at" ON "reminder_event" ("scheduled_at");
CREATE INDEX "sent_at" ON "reminder_event" ("sent_at");

CREATE TABLE "schedule" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "g_schedule_id" bigint NOT NULL,
    "mailbox_id" bigint NOT NULL
);
CREATE INDEX "schedule_customer_id" ON "schedule" ("customer_id");
CREATE INDEX "g_schedule_id" ON "schedule" ("g_schedule_id");
CREATE INDEX "schedule_mailbox_id" ON "schedule" ("mailbox_id");

CREATE TABLE "sent_email" (
    "id" SERIAL PRIMARY KEY,
    "customer_id" bigint,
    "from_name" varchar(255) NOT NULL,
    "from_email" varchar(255) NOT NULL,
    "to_name" varchar(255) NOT NULL,
    "to_email" varchar(255) NOT NULL,
    "preferred_language" varchar(2) NOT NULL,
    "template_name" varchar(255) NOT NULL,
    "language" varchar(2) NOT NULL,
    "parameters" text NOT NULL,
    "bcc_email" varchar(255) NOT NULL,
    "subject" varchar(255) NOT NULL,
    "body" text NOT NULL,
    "status" varchar(255) NOT NULL,
    "status_message" varchar(255) NOT NULL,
    "created_at" timestamp NOT NULL
);
CREATE INDEX "sent_email_customer_id" ON "sent_email" ("customer_id");
CREATE INDEX "to_email" ON "sent_email" ("to_email");

CREATE TABLE "thread" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "folder_id" bigint NOT NULL,
    "g_thread_id" bigint NOT NULL,
    "read_at" timestamp,
    "deleted_at" timestamp
);
CREATE INDEX "thread_customer_id" ON "thread" ("customer_id");
CREATE INDEX "folder_id" ON "thread" ("folder_id");
CREATE INDEX "thread_g_thread_id" ON "thread" ("g_thread_id");

CREATE TABLE "token" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "user_id" bigint NOT NULL,
    "name" varchar(255) NOT NULL,
    "value" varchar(255) NOT NULL UNIQUE,
    "created_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "validated_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "used_at" timestamp,
    "valid_until" timestamp
);
CREATE INDEX "token_customer_id" ON "token" ("customer_id");
CREATE INDEX "token_user_id" ON "token" ("user_id");

CREATE TABLE "typing_indicator" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "g_chat_id" bigint NOT NULL,
    "user_id" bigint NOT NULL,
    "started_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "expires_at" timestamp NOT NULL
);
CREATE INDEX "typing_indicator_customer_id" ON "typing_indicator" ("customer_id");
CREATE INDEX "expires_at" ON "typing_indicator" ("expires_at");
CREATE INDEX "typing_indicator_g_chat_id" ON "typing_indicator" ("g_chat_id");
CREATE INDEX "typing_indicator_user_id" ON "typing_indicator" ("user_id");

CREATE TABLE "user" (
    "id" BIGSERIAL PRIMARY KEY,
    "customer_id" bigint NOT NULL,
    "username" varchar(255) NOT NULL,
    "password" varchar(255) NOT NULL,
    "name" varchar(255) NOT NULL,
    "is_admin" integer NOT NULL DEFAULT 0,
    "created_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "modified_at" timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX "user_customer_id" ON "user" ("customer_id");

ALTER TABLE "alias" ADD CONSTRAINT "fk_alias_mailbox_id" FOREIGN KEY ("mailbox_id") REFERENCES "mailbox" ("id") ON DELETE CASCADE;
ALTER TABLE "alias" ADD CONSTRAINT "fk_alias_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "attachment" ADD CONSTRAINT "fk_attachment_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "attachment" ADD CONSTRAINT "fk_attachment_message_id" FOREIGN KEY ("message_id") REFERENCES "message" ("id") ON DELETE CASCADE;
ALTER TABLE "attachment" ADD CONSTRAINT "fk_attachment_g_attachment_id" FOREIGN KEY ("g_attachment_id") REFERENCES "g_attachment" ("id") ON DELETE CASCADE;
ALTER TABLE "audit_log" ADD CONSTRAINT "fk_audit_log_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "audit_log" ADD CONSTRAINT "fk_audit_log_user_id" FOREIGN KEY ("user_id") REFERENCES "user" ("id") ON DELETE CASCADE;
ALTER TABLE "chat" ADD CONSTRAINT "fk_chat_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "chat" ADD CONSTRAINT "fk_chat_mailbox_id" FOREIGN KEY ("mailbox_id") REFERENCES "mailbox" ("id") ON DELETE CASCADE;
ALTER TABLE "chat" ADD CONSTRAINT "fk_chat_g_chat_id" FOREIGN KEY ("g_chat_id") REFERENCES "g_chat" ("id") ON DELETE CASCADE;
ALTER TABLE "chat" ADD CONSTRAINT "fk_chat_last_read_message_id" FOREIGN KEY ("last_read_message_id") REFERENCES "g_chat_message" ("id") ON DELETE SET NULL;
ALTER TABLE "chat_message" ADD CONSTRAINT "fk_chat_message_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "chat_message" ADD CONSTRAINT "fk_chat_message_chat_id" FOREIGN KEY ("chat_id") REFERENCES "chat" ("id") ON DELETE CASCADE;
ALTER TABLE "chat_message" ADD CONSTRAINT "fk_chat_message_g_chat_message_id" FOREIGN KEY ("g_chat_message_id") REFERENCES "g_chat_message" ("id") ON DELETE CASCADE;
ALTER TABLE "contact" ADD CONSTRAINT "fk_contact_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "customer" ADD CONSTRAINT "fk_customer_default_mailbox_id" FOREIGN KEY ("default_mailbox_id") REFERENCES "mailbox" ("id") ON DELETE SET NULL;
ALTER TABLE "domain" ADD CONSTRAINT "fk_domain_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "folder" ADD CONSTRAINT "fk_folder_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "folder" ADD CONSTRAINT "fk_folder_mailbox_id" FOREIGN KEY ("mailbox_id") REFERENCES "mailbox" ("id") ON DELETE CASCADE;
ALTER TABLE "g_attachment" ADD CONSTRAINT "fk_g_attachment_g_message_id" FOREIGN KEY ("g_message_id") REFERENCES "g_message" ("id") ON DELETE CASCADE;
ALTER TABLE "g_attendee" ADD CONSTRAINT "fk_g_attendee_g_meeting_id" FOREIGN KEY ("g_meeting_id") REFERENCES "g_meeting" ("id") ON DELETE CASCADE;
ALTER TABLE "g_chat_attachment" ADD CONSTRAINT "fk_g_chat_attachment_g_chat_message_id" FOREIGN KEY ("g_chat_message_id") REFERENCES "g_chat_message" ("id") ON DELETE CASCADE;
ALTER TABLE "g_chat_message" ADD CONSTRAINT "fk_g_chat_message_g_chat_id" FOREIGN KEY ("g_chat_id") REFERENCES "g_chat" ("id") ON DELETE CASCADE;
ALTER TABLE "g_chat_participant" ADD CONSTRAINT "fk_g_chat_participant_g_chat_id" FOREIGN KEY ("g_chat_id") REFERENCES "g_chat" ("id") ON DELETE CASCADE;
ALTER TABLE "g_document" ADD CONSTRAINT "fk_g_document_g_meeting_id" FOREIGN KEY ("g_meeting_id") REFERENCES "g_meeting" ("id") ON DELETE CASCADE;
ALTER TABLE "g_meeting" ADD CONSTRAINT "fk_g_meeting_g_schedule" FOREIGN KEY ("g_schedule") REFERENCES "g_schedule" ("id");
ALTER TABLE "g_message" ADD CONSTRAINT "fk_g_message_g_thread_id" FOREIGN KEY ("g_thread_id") REFERENCES "g_thread" ("id") ON DELETE CASCADE;
ALTER TABLE "g_subscription" ADD CONSTRAINT "fk_g_subscription_g_thread_id" FOREIGN KEY ("g_thread_id") REFERENCES "g_thread" ("id") ON DELETE CASCADE;
ALTER TABLE "mailbox" ADD CONSTRAINT "fk_mailbox_default_folder_id" FOREIGN KEY ("default_folder_id") REFERENCES "folder" ("id") ON DELETE SET NULL;
ALTER TABLE "mailbox" ADD CONSTRAINT "fk_mailbox_default_alias_id" FOREIGN KEY ("default_alias_id") REFERENCES "alias" ("id") ON DELETE SET NULL;
ALTER TABLE "mailbox" ADD CONSTRAINT "fk_mailbox_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "mailbox" ADD CONSTRAINT "fk_mailbox_user_id" FOREIGN KEY ("user_id") REFERENCES "user" ("id") ON DELETE SET NULL;
ALTER TABLE "meeting" ADD CONSTRAINT "fk_meeting_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "meeting" ADD CONSTRAINT "fk_meeting_mailbox_id" FOREIGN KEY ("mailbox_id") REFERENCES "mailbox" ("id") ON DELETE CASCADE;
ALTER TABLE "meeting" ADD CONSTRAINT "fk_meeting_schedule_id" FOREIGN KEY ("schedule_id") REFERENCES "schedule" ("id");
ALTER TABLE "meeting" ADD CONSTRAINT "fk_meeting_g_meeting_id" FOREIGN KEY ("g_meeting_id") REFERENCES "g_meeting" ("id") ON DELETE CASCADE;
ALTER TABLE "message" ADD CONSTRAINT "fk_message_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "message" ADD CONSTRAINT "fk_message_thread_id" FOREIGN KEY ("thread_id") REFERENCES "thread" ("id") ON DELETE CASCADE;
ALTER TABLE "message" ADD CONSTRAINT "fk_message_g_message_id" FOREIGN KEY ("g_message_id") REFERENCES "g_message" ("id") ON DELETE CASCADE;
ALTER TABLE "message_status" ADD CONSTRAINT "fk_message_status_g_chat_message_id" FOREIGN KEY ("g_chat_message_id") REFERENCES "g_chat_message" ("id") ON DELETE CASCADE;
ALTER TABLE "notification_preference" ADD CONSTRAINT "fk_notification_preference_user_id" FOREIGN KEY ("user_id") REFERENCES "user" ("id") ON DELETE CASCADE;
ALTER TABLE "presence_status" ADD CONSTRAINT "fk_presence_status_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "presence_status" ADD CONSTRAINT "fk_presence_status_user_id" FOREIGN KEY ("user_id") REFERENCES "user" ("id") ON DELETE CASCADE;
ALTER TABLE "reminder_event" ADD CONSTRAINT "fk_reminder_event_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "reminder_event" ADD CONSTRAINT "fk_reminder_event_mailbox_id" FOREIGN KEY ("mailbox_id") REFERENCES "mailbox" ("id") ON DELETE CASCADE;
ALTER TABLE "reminder_event" ADD CONSTRAINT "fk_reminder_event_g_meeting_id" FOREIGN KEY ("g_meeting_id") REFERENCES "g_meeting" ("id") ON DELETE CASCADE;
ALTER TABLE "schedule" ADD CONSTRAINT "fk_schedule_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "schedule" ADD CONSTRAINT "fk_schedule_mailbox_id" FOREIGN KEY ("mailbox_id") REFERENCES "mailbox" ("id") ON DELETE CASCADE;
ALTER TABLE "schedule" ADD CONSTRAINT "fk_schedule_g_schedule_id" FOREIGN KEY ("g_schedule_id") REFERENCES "g_schedule" ("id") ON DELETE CASCADE;
ALTER TABLE "sent_email" ADD CONSTRAINT "fk_sent_email_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "thread" ADD CONSTRAINT "fk_thread_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "thread" ADD CONSTRAINT "fk_thread_folder_id" FOREIGN KEY ("folder_id") REFERENCES "folder" ("id") ON DELETE CASCADE;
ALTER TABLE "thread" ADD CONSTRAINT "fk_thread_g_thread_id" FOREIGN KEY ("g_thread_id") REFERENCES "g_thread" ("id") ON DELETE CASCADE;
ALTER TABLE "token" ADD CONSTRAINT "fk_token_user_id" FOREIGN KEY ("user_id") REFERENCES "user" ("id") ON DELETE CASCADE;
ALTER TABLE "token" ADD CONSTRAINT "fk_token_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "typing_indicator" ADD CONSTRAINT "fk_typing_indicator_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;
ALTER TABLE "typing_indicator" ADD CONSTRAINT "fk_typing_indicator_g_chat_id" FOREIGN KEY ("g_chat_id") REFERENCES "g_chat" ("id") ON DELETE CASCADE;
ALTER TABLE "typing_indicator" ADD CONSTRAINT "fk_typing_indicator_user_id" FOREIGN KEY ("user_id") REFERENCES "user" ("id") ON DELETE CASCADE;
ALTER TABLE "user" ADD CONSTRAINT "fk_user_customer_id" FOREIGN KEY ("customer_id") REFERENCES "customer" ("id") ON DELETE CASCADE;

COMMENT ON COLUMN "g_subscription"."refused_at" IS 'Set when the address was refused as unknown; it is then not a member (RFC 6.5)';
COMMENT ON COLUMN "g_subscription"."left_at" IS 'Set when the member left the thread; they are then not a member (RFC 5.6)';

