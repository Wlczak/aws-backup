// Package db owns the sqlite index: schema, migrations, and typed queries.
//
// Uses modernc.org/sqlite (pure Go) so cross-compiling needs no CGO.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// DB wraps *sql.DB with a stable, typed query surface.
type DB struct {
	*sql.DB
}

// Open returns a ready-to-use DB connected to path (":memory:" works for tests).
// It runs the embedded schema and enables WAL journaling for on-disk files.
func Open(ctx context.Context, path string) (*DB, error) {
	sqldb, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	sqldb.SetMaxOpenConns(1) // modernc sqlite is safer with a single writer
	if err := sqldb.PingContext(ctx); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}

	if path != ":memory:" {
		if _, err := sqldb.ExecContext(ctx, "PRAGMA journal_mode=WAL;"); err != nil {
			sqldb.Close()
			return nil, fmt.Errorf("enable WAL: %w", err)
		}
	}
	if _, err := sqldb.ExecContext(ctx, "PRAGMA foreign_keys=ON;"); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	if _, err := sqldb.ExecContext(ctx, schemaSQL); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	return &DB{DB: sqldb}, nil
}

// Checkpoint flushes the WAL file into the main database file so the
// .db can be uploaded to S3 as a consistent snapshot. Safe to call while
// the database is open; other connections continue to work normally.
func (db *DB) Checkpoint(ctx context.Context) error {
	_, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE);")
	return err
}

// isoTime formats t in RFC3339Nano so it sorts lexicographically.
func isoTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseTime is the inverse of isoTime; zero value on empty input.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
