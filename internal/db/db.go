// Package db owns the sqlite index: schema, migrations, and typed queries.
//
// Uses github.com/glebarez/sqlite (pure Go, no CGO) so cross-compiling
// works without a C toolchain. Schema management is handled by GORM's
// AutoMigrate, removing the need for an embedded schema.sql.
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
	}
	if err := gdb.WithContext(ctx).Exec("PRAGMA foreign_keys=ON").Error; err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	if err := gdb.WithContext(ctx).AutoMigrate(&File{}, &Run{}, &RunLog{}, &Setting{}); err != nil {
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
