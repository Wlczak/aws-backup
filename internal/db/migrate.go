package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsDir is the directory inside migrationsFS where goose looks
// for `*.sql` files. Kept as a constant so a future migration ever
// living somewhere else doesn't require touching the runner.
const migrationsDir = "migrations"

// baselineVersion is the version of the schema captured by
// 00001_baseline.sql. A pre-existing DB that was created by
// AutoMigrate (before this branch) is functionally identical to a
// fresh DB at this version, so we stamp without re-running.
const baselineVersion int64 = 1

// runMigrations applies any pending goose migrations to db. If the
// DB pre-existed under the old AutoMigrate regime — `files` table is
// already present but the goose tracking table is not — it stamps the
// baseline as applied without re-running it, then proceeds with the
// rest. New DBs run every migration from 0.
func runMigrations(ctx context.Context, db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}

	preExisting, err := needsBaselineStamp(ctx, db)
	if err != nil {
		return err
	}
	if preExisting {
		// EnsureDBVersion creates the goose tracking table if missing,
		// then InsertVersion records the baseline as applied so
		// goose.UpContext skips it.
		if _, err := goose.EnsureDBVersionContext(ctx, db); err != nil {
			return fmt.Errorf("goose ensure tracking table: %w", err)
		}
		if err := stampVersion(ctx, db, baselineVersion); err != nil {
			return fmt.Errorf("goose stamp baseline: %w", err)
		}
	}

	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// needsBaselineStamp returns true when the DB was created by the old
// AutoMigrate path: the application schema is in place (`files` table
// exists) but the goose tracking table is not. A truly empty DB will
// have neither and goose will create everything from migration 1.
func needsBaselineStamp(ctx context.Context, db *sql.DB) (bool, error) {
	hasFiles, err := tableExists(ctx, db, "files")
	if err != nil {
		return false, err
	}
	hasGoose, err := tableExists(ctx, db, "goose_db_version")
	if err != nil {
		return false, err
	}
	return hasFiles && !hasGoose, nil
}

func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var got string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&got)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return got == name, nil
}

// stampVersion records `version` as applied in goose's tracking table
// without running its SQL. Used to catch up DBs that pre-date goose.
func stampVersion(ctx context.Context, db *sql.DB, version int64) error {
	// Idempotent: if the row's already there (re-run after a
	// successful stamp), don't double-write.
	current, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return err
	}
	if current >= version {
		return nil
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (?, 1, CURRENT_TIMESTAMP)`,
		version,
	)
	return err
}
