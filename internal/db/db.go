// Package db owns the sqlite index: schema, migrations, and typed queries.
//
// Uses github.com/glebarez/sqlite (pure Go, no CGO) so cross-compiling
// works without a C toolchain. Schema management goes through goose,
// with `*.sql` migrations embedded under `migrations/`. GORM tags on
// the model structs are advisory (they document column types and let
// the typed query builder map fields) but do not run AutoMigrate —
// schema lives in the migrations.
package db

import (
	"context"
	"fmt"

	gsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB wraps *gorm.DB with a stable, typed query surface.
type DB struct {
	g *gorm.DB
}

// Open returns a ready-to-use DB connected to path (":memory:" works for tests).
// WAL mode and foreign keys are enabled for on-disk databases.
func Open(ctx context.Context, path string) (*DB, error) {
	gdb, err := gorm.Open(gsqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if path != ":memory:" {
		if err := gdb.WithContext(ctx).Exec("PRAGMA journal_mode=WAL").Error; err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("enable WAL: %w", err)
		}
		// synchronous=NORMAL is the documented safe pairing with WAL: a
		// power loss can drop the most recent commits but cannot corrupt
		// the database, and we trade fsync-per-commit for one fsync per
		// checkpoint. Big scans (UpsertFileBatch flushes every 3 s) are
		// 2–3× faster at this setting because each commit no longer
		// stalls on the kernel write barrier. The index is a derived
		// view of the bucket and is rebuilt from the cloud on demand
		// anyway, so the durability trade is acceptable. (#scan-batch)
		if err := gdb.WithContext(ctx).Exec("PRAGMA synchronous=NORMAL").Error; err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("set synchronous=NORMAL: %w", err)
		}
	}
	if err := gdb.WithContext(ctx).Exec("PRAGMA busy_timeout=5000").Error; err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}
	if err := gdb.WithContext(ctx).Exec("PRAGMA foreign_keys=ON").Error; err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	if err := runMigrations(ctx, sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &DB{g: gdb}, nil
}

// Close releases the database connection.
func (db *DB) Close() error {
	sqlDB, err := db.g.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Checkpoint flushes the WAL file into the main database file so the
// .db can be uploaded to S3 as a consistent snapshot.
func (db *DB) Checkpoint(ctx context.Context) error {
	return db.g.WithContext(ctx).Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error
}
