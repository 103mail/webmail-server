-- Full-text search indexes for PostgreSQL (ADR 19). DBML cannot describe
-- these, so they are applied after webmail.postgresql.sql.
--
-- Each searched table gets a stored generated tsvector column, "search", with a
-- GIN index. The text has its punctuation turned into spaces before it is
-- parsed, so an address such as alice@example.com indexes as the words alice,
-- example and com, exactly as SQLite's unicode61 tokenizer splits it, rather
-- than as one email token. The simple configuration lowercases and does not
-- stem or drop stopwords. internal/store renders queries against this.

ALTER TABLE "g_thread" ADD COLUMN "search" tsvector GENERATED ALWAYS AS
    (to_tsvector('simple', regexp_replace("subject_line", '[[:punct:]]', ' ', 'g'))) STORED;
CREATE INDEX "g_thread_search" ON "g_thread" USING GIN ("search");

ALTER TABLE "g_message" ADD COLUMN "search" tsvector GENERATED ALWAYS AS
    (to_tsvector('simple', regexp_replace("body" || ' ' || "sender_name", '[[:punct:]]', ' ', 'g'))) STORED;
CREATE INDEX "g_message_search" ON "g_message" USING GIN ("search");

ALTER TABLE "contact" ADD COLUMN "search" tsvector GENERATED ALWAYS AS
    (to_tsvector('simple', regexp_replace("name" || ' ' || "email", '[[:punct:]]', ' ', 'g'))) STORED;
CREATE INDEX "contact_search" ON "contact" USING GIN ("search");

ALTER TABLE "g_chat_message" ADD COLUMN "search" tsvector GENERATED ALWAYS AS
    (to_tsvector('simple', regexp_replace("body" || ' ' || "sender_name", '[[:punct:]]', ' ', 'g'))) STORED;
CREATE INDEX "g_chat_message_search" ON "g_chat_message" USING GIN ("search");
