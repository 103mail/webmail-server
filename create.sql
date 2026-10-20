-- Create the PostgreSQL role and database the server runs on (ADR 19). The
-- server creates the schema itself on its first start against the empty
-- database. Run as the postgres superuser:
--
--     sudo -u postgres psql < create.sql
CREATE ROLE "webmail" LOGIN PASSWORD 'webmail';
CREATE DATABASE "webmail" OWNER "webmail" ENCODING 'UTF8' TEMPLATE "template0";
