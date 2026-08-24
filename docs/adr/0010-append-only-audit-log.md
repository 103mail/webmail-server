# 10. Append-only audit log with YAML diffs

Status: accepted
Date: 2026-08-24
Source: `../webmail/specs/004-audit-logging/spec.md` and `data-model.md`

## Context

The system makes strong claims: messages are not deleted, content is not emailed,
only subscribers can read a thread. Those claims are worth little without a
record that can show they held. The record is also what makes a retention purge
defensible, since a purge is the one operation that legitimately destroys
content.

A record that the implicated party can edit proves nothing, and a record that
contains message bodies would undo ADR 4 by putting content somewhere it was
never supposed to be.

## Decision

One `audit_log` table: `id`, `customer_id`, `user_id`, `username`, `is_admin`,
`created_at`, `operation`, `entity`, `identifier`, `changes_before`,
`changes_after`.

`operation` is one of `insert`, `update`, `delete`. Entity type and identifier
supply the rest of the context, so no per-feature operation vocabulary is needed.
For an insert, `changes_after` holds the whole record and `changes_before` is
empty. For an update, both hold only the changed fields. For a delete,
`changes_before` holds the whole record and `changes_after` is empty. Both are
YAML, matching the PHP implementation.

Entries are append-only. No update or delete except an authorised retention
purge, which is itself logged. `username` is denormalised so entries survive user
deletion.

Sensitive values are sanitised before writing: passwords never in plaintext,
tokens never in plaintext, message bodies replaced by `REDACTED`, event
descriptions reduced to metadata.

`customer_id` isolates tenants. Admins query only their own tenant. Reading the
audit log is itself audited.

Audit writes must not block or fail the operation they describe. A failed write is
queued and retried.

## Consequences

- Append-only has to be enforced at the database level where possible, since
  application-level enforcement is exactly what an attacker with database access
  bypasses.
- Indexes on `(customer_id, created_at)`, `(customer_id, user_id, created_at)`
  and `(customer_id, operation, created_at)` are required to meet the 2 second
  query target.
- Writing audit entries asynchronously means an operation can succeed while its
  audit entry is still queued. The gap is bounded by the retry, and the 5 second
  target in the spec accounts for it.
- Hash chaining for tamper evidence is not decided. It is listed as an open
  question in the source spec and is left for a future ADR.
