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
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	gsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB wraps *gorm.DB with a stable, typed query surface.
type DB struct {
	g            *gorm.DB
	fileRevision atomic.Uint64
}

const defaultConnIdleTimeout = 15 * time.Minute

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
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxIdleTime(defaultConnIdleTimeout)
	// database/sql will close the underlying sqlite connection after the
	// pool has been idle for 15 minutes, and it will transparently open a
	// fresh connection on the next query. The app still keeps the DB handle
	// itself open for the process lifetime; only the idle connection goes away.

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

	db := &DB{g: gdb}
	if err := db.registerFileRevisionCallbacks(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("register file revision callbacks: %w", err)
	}
	return db, nil
}

// FileRevision returns the process-local version of the files table. Every
// successful GORM create, update, or delete advances it so API caches can
// reject stale entries without coupling every writer to the HTTP package.
func (db *DB) FileRevision() uint64 { return db.fileRevision.Load() }

func (db *DB) registerFileRevisionCallbacks() error {
	bump := func(tx *gorm.DB) {
		if tx.Error != nil || tx.RowsAffected == 0 || tx.Statement == nil {
			return
		}
		table := tx.Statement.Table
		if table == "" && tx.Statement.Schema != nil {
			table = tx.Statement.Schema.Table
		}
		if table == "files" {
			db.fileRevision.Add(1)
		}
	}
	if err := db.g.Callback().Create().After("gorm:create").Register("aws_backup:file_revision", bump); err != nil {
		return err
	}
	if err := db.g.Callback().Update().After("gorm:update").Register("aws_backup:file_revision", bump); err != nil {
		return err
	}
	return db.g.Callback().Delete().After("gorm:delete").Register("aws_backup:file_revision", bump)
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

// SnapshotTo writes a consistent copy of the live database to dst using
// SQLite's VACUUM INTO. The resulting file can be uploaded or opened
// independently of concurrent writes to the live DB handle.
func (db *DB) SnapshotTo(ctx context.Context, dst string) error {
	if dst == "" {
		return errors.New("snapshot path is required")
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	escaped := strings.ReplaceAll(dst, "'", "''")
	return db.g.WithContext(ctx).Exec("VACUUM INTO '" + escaped + "'").Error
}
