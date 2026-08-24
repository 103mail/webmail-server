# 12. Schema kept in DBML

Status: accepted
Date: 2026-08-24
Source: this repo; tooling from `../dbml-tools`

Note: this is a new decision for the Go backend, not inherited from the PHP repo.

## Context

The schema is shared with the PHP backend for the duration of the migration
(ADR 2), so both must agree on it exactly. It is also large: canonical tables,
projection tables and supporting tables across mail, calendar, chat and audit,
with invariants that only make sense when the whole shape is visible.

`../webmail` keeps only `create.sql`, which creates the database and user but not
the tables. The actual schema lives in the running database. That makes review
impossible and migration a manual exercise.

## Decision

Keep the schema as DBML in this repo and treat it as the source of truth. Use
`../dbml-tools` to work with it: `check` to validate, `todbml` to import the
existing schema from a DSN, `tosql` to generate `CREATE TABLE` statements,
`migrate` to diff two schemas into migration SQL, and `todot` for diagrams.

Table names are singular snake_case with canonical tables prefixed `g_`, matching
constitution principle IX and therefore matching the PHP database.

## Consequences

- Schema changes are reviewable diffs, and the projection structure is legible in
  one file rather than reconstructed from a live database.
- Migrations are generated from a diff rather than written by hand, which removes
  a class of mistakes but means generated SQL still needs review before it runs
  against a database holding immutable history.
- The initial DBML is imported from the production PHP database with `todbml`, so
  the two backends start in agreement.
- This adds a build-time dependency on a sibling repo. If that becomes awkward,
  vendor the generated SQL rather than the tool.
