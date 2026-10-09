package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Append migrations; never edit an already released migration.
var migrations = [][]string{{
	`CREATE TABLE deliveries (
 sequence INTEGER PRIMARY KEY,
 delivery_id TEXT NOT NULL UNIQUE CHECK(length(delivery_id) > 0),
 event_name TEXT NOT NULL CHECK(length(event_name) > 0),
 payload BLOB NOT NULL CHECK(length(payload) > 0),
 received_at TEXT NOT NULL,
 accepted_resource BLOB NOT NULL
 )`,
	`CREATE TABLE processing (
 delivery_id TEXT PRIMARY KEY NOT NULL REFERENCES deliveries(delivery_id),
 status TEXT NOT NULL CHECK(status IN ('pending','processing','retryable','completed','failed')),
 attempts INTEGER NOT NULL CHECK(attempts >= 0),
 next_attempt_at TEXT,
 error_category TEXT NOT NULL
 )`,
	`CREATE TABLE resources (
 delivery_id TEXT PRIMARY KEY NOT NULL REFERENCES deliveries(delivery_id),
 owner TEXT NOT NULL,
 repository TEXT NOT NULL,
 kind TEXT NOT NULL,
 number INTEGER NOT NULL CHECK(number > 0),
 node_id TEXT NOT NULL
 )`,
	`CREATE INDEX resource_work ON resources(owner, repository, kind, number)`,
	`CREATE TABLE provenance (
 namespace TEXT NOT NULL CHECK(length(namespace) > 0),
 stable_key TEXT NOT NULL CHECK(length(stable_key) > 0),
 delivery_id TEXT NOT NULL REFERENCES deliveries(delivery_id),
 metadata BLOB NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY(namespace, stable_key)
 )`,
}}

func initialize(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 || version > len(migrations) {
		return fmt.Errorf("%w: version %d", ErrSchema, version)
	}
	if version == 0 {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("%w: unversioned database is not empty", ErrSchema)
		}
	}
	for i := version; i < len(migrations); i++ {
		for _, statement := range migrations[i] {
			if _, err = tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("%w: migration %d: %w", ErrSchema, i+1, err)
			}
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return err
		}
	}
	// Version alone is insufficient: reject changed/missing/unknown schema objects.
	expected := make(map[string]bool)
	for _, migration := range migrations {
		for _, statement := range migration {
			expected[statement] = true
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT sql FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'")
	if err != nil {
		return err
	}
	for rows.Next() {
		var statement string
		if err = rows.Scan(&statement); err != nil {
			rows.Close()
			return err
		}
		statement = strings.TrimSpace(statement)
		if !expected[statement] {
			rows.Close()
			return fmt.Errorf("%w: unexpected schema object", ErrSchema)
		}
		delete(expected, statement)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(expected) != 0 {
		return fmt.Errorf("%w: missing schema objects", ErrSchema)
	}
	return tx.Commit()
}
