package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/mevdschee/webmail-server/schema"
)

// CreateSchema creates the tables and search indexes in an empty database and
// reports whether it did. A database that already has the user table is left
// alone: changing an existing schema is the job of migrations, not of startup.
//
// It is what makes a Domain cheap to create: point the server at a new SQLite
// file and it is ready (ADR 19). On PostgreSQL the DDL is transactional, so a
// failure leaves the database as empty as it was.
func (db *DB) CreateSchema(ctx context.Context) (bool, error) {
	exists, err := db.tableExists(ctx, "user")
	if err != nil {
		return false, fmt.Errorf("inspect schema: %w", err)
	}
	if exists {
		return false, nil
	}
	ddl := []string{schema.PostgreSQL, schema.SearchPostgreSQL}
	if db.Engine == SQLite {
		ddl = []string{schema.SQLite, schema.SearchSQLite}
	}
	err = db.Tx(ctx, func(tx *sql.Tx) error {
		for _, script := range ddl {
			if _, err := tx.ExecContext(ctx, script); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("create schema: %w", err)
	}
	return true, nil
}

func (db *DB) tableExists(ctx context.Context, table string) (bool, error) {
	query := `SELECT COUNT(*) FROM "information_schema"."tables" ` +
		`WHERE "table_schema" = current_schema() AND "table_name" = ?`
	if db.Engine == SQLite {
		query = `SELECT COUNT(*) FROM "sqlite_master" WHERE "type" = 'table' AND "name" = ?`
	}
	n, err := SelectInt(ctx, db, query, table)
	return n > 0, err
}
