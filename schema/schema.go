// Package schema holds the DDL the server runs on, embedded so a binary can
// create its own database (ADR 19). webmail.dbml is the source; the per-engine
// files are generated from it by generate.sh, and the search indexes, which
// DBML cannot describe, are written by hand.
package schema

import _ "embed"

var (
	//go:embed webmail.postgresql.sql
	PostgreSQL string
	//go:embed search.postgresql.sql
	SearchPostgreSQL string
	//go:embed webmail.sqlite.sql
	SQLite string
	//go:embed search.sqlite.sql
	SearchSQLite string
)
