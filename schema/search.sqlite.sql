-- Full-text search indexes for SQLite (ADR 19). DBML cannot describe these,
-- so they are applied after webmail.sqlite.sql.
--
-- Each searched table gets an FTS5 external-content table, "<table>_fts",
-- whose rowid is the table's id. Triggers keep it current, so the text is
-- stored once, in the table itself. unicode61 splits on everything but letters
-- and digits and lowercases, and remove_diacritics 0 keeps accents
-- significant, as PostgreSQL's simple configuration does. internal/store
-- renders queries against this.

CREATE VIRTUAL TABLE "g_thread_fts" USING fts5("subject_line",
    content='g_thread', content_rowid='id', tokenize='unicode61 remove_diacritics 0');
CREATE TRIGGER "g_thread_fts_insert" AFTER INSERT ON "g_thread" BEGIN
    INSERT INTO "g_thread_fts" ("rowid", "subject_line") VALUES (new."id", new."subject_line");
END;
CREATE TRIGGER "g_thread_fts_delete" AFTER DELETE ON "g_thread" BEGIN
    INSERT INTO "g_thread_fts" ("g_thread_fts", "rowid", "subject_line") VALUES ('delete', old."id", old."subject_line");
END;
CREATE TRIGGER "g_thread_fts_update" AFTER UPDATE OF "subject_line" ON "g_thread" BEGIN
    INSERT INTO "g_thread_fts" ("g_thread_fts", "rowid", "subject_line") VALUES ('delete', old."id", old."subject_line");
    INSERT INTO "g_thread_fts" ("rowid", "subject_line") VALUES (new."id", new."subject_line");
END;
-- Index whatever the table already holds, such as seeded rows.
INSERT INTO "g_thread_fts" ("g_thread_fts") VALUES ('rebuild');

CREATE VIRTUAL TABLE "g_message_fts" USING fts5("body", "sender_name",
    content='g_message', content_rowid='id', tokenize='unicode61 remove_diacritics 0');
CREATE TRIGGER "g_message_fts_insert" AFTER INSERT ON "g_message" BEGIN
    INSERT INTO "g_message_fts" ("rowid", "body", "sender_name") VALUES (new."id", new."body", new."sender_name");
END;
CREATE TRIGGER "g_message_fts_delete" AFTER DELETE ON "g_message" BEGIN
    INSERT INTO "g_message_fts" ("g_message_fts", "rowid", "body", "sender_name") VALUES ('delete', old."id", old."body", old."sender_name");
END;
CREATE TRIGGER "g_message_fts_update" AFTER UPDATE OF "body", "sender_name" ON "g_message" BEGIN
    INSERT INTO "g_message_fts" ("g_message_fts", "rowid", "body", "sender_name") VALUES ('delete', old."id", old."body", old."sender_name");
    INSERT INTO "g_message_fts" ("rowid", "body", "sender_name") VALUES (new."id", new."body", new."sender_name");
END;
-- Index whatever the table already holds, such as seeded rows.
INSERT INTO "g_message_fts" ("g_message_fts") VALUES ('rebuild');

CREATE VIRTUAL TABLE "contact_fts" USING fts5("name", "email",
    content='contact', content_rowid='id', tokenize='unicode61 remove_diacritics 0');
CREATE TRIGGER "contact_fts_insert" AFTER INSERT ON "contact" BEGIN
    INSERT INTO "contact_fts" ("rowid", "name", "email") VALUES (new."id", new."name", new."email");
END;
CREATE TRIGGER "contact_fts_delete" AFTER DELETE ON "contact" BEGIN
    INSERT INTO "contact_fts" ("contact_fts", "rowid", "name", "email") VALUES ('delete', old."id", old."name", old."email");
END;
CREATE TRIGGER "contact_fts_update" AFTER UPDATE OF "name", "email" ON "contact" BEGIN
    INSERT INTO "contact_fts" ("contact_fts", "rowid", "name", "email") VALUES ('delete', old."id", old."name", old."email");
    INSERT INTO "contact_fts" ("rowid", "name", "email") VALUES (new."id", new."name", new."email");
END;
-- Index whatever the table already holds, such as seeded rows.
INSERT INTO "contact_fts" ("contact_fts") VALUES ('rebuild');

CREATE VIRTUAL TABLE "g_chat_message_fts" USING fts5("body", "sender_name",
    content='g_chat_message', content_rowid='id', tokenize='unicode61 remove_diacritics 0');
CREATE TRIGGER "g_chat_message_fts_insert" AFTER INSERT ON "g_chat_message" BEGIN
    INSERT INTO "g_chat_message_fts" ("rowid", "body", "sender_name") VALUES (new."id", new."body", new."sender_name");
END;
CREATE TRIGGER "g_chat_message_fts_delete" AFTER DELETE ON "g_chat_message" BEGIN
    INSERT INTO "g_chat_message_fts" ("g_chat_message_fts", "rowid", "body", "sender_name") VALUES ('delete', old."id", old."body", old."sender_name");
END;
CREATE TRIGGER "g_chat_message_fts_update" AFTER UPDATE OF "body", "sender_name" ON "g_chat_message" BEGIN
    INSERT INTO "g_chat_message_fts" ("g_chat_message_fts", "rowid", "body", "sender_name") VALUES ('delete', old."id", old."body", old."sender_name");
    INSERT INTO "g_chat_message_fts" ("rowid", "body", "sender_name") VALUES (new."id", new."body", new."sender_name");
END;
-- Index whatever the table already holds, such as seeded rows.
INSERT INTO "g_chat_message_fts" ("g_chat_message_fts") VALUES ('rebuild');
