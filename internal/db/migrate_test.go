package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestMigrationsFreshDB applies every migration to an empty database
// and verifies the goose tracking table has a row at the highest
// migration version.
func TestMigrationsFreshDB(t *testing.T) {
	d := openTestDB(t)

	sqlDB, err := d.g.DB()
	if err != nil {
		t.Fatalf("get sql.DB: %v", err)
	}
	got, err := goose.GetDBVersionContext(context.Background(), sqlDB)
	if err != nil {
		t.Fatalf("GetDBVersion: %v", err)
	}
	want := highestEmbeddedMigrationVersion(t)
	if got != want {
		t.Errorf("goose version got=%d want=%d", got, want)
	}
}

// TestMigrationsStampsExistingAutoMigrateDB simulates a database
// created by the pre-goose AutoMigrate path: a `files` table exists
// but no `goose_db_version`. Open should stamp the baseline (so
// migration 1 isn't re-run, which would fail or duplicate state) and
// then apply any newer migrations.
func TestMigrationsStampsExistingAutoMigrateDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// Hand-bake a "legacy AutoMigrate" DB by running the baseline
	// schema directly — no goose tracking table.
	{
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open raw: %v", err)
		}
		if _, err := raw.Exec(loadMigration(t, "00001_baseline.sql")); err != nil {
			t.Fatalf("seed legacy schema: %v", err)
		}
		if err := raw.Close(); err != nil {
			t.Fatalf("close raw: %v", err)
		}
	}

	// Open via the real path — should detect pre-existing schema,
	// stamp baseline, then apply 00002.
	d, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	sqlDB, _ := d.g.DB()
	got, err := goose.GetDBVersionContext(context.Background(), sqlDB)
	if err != nil {
		t.Fatalf("GetDBVersion: %v", err)
	}
	want := highestEmbeddedMigrationVersion(t)
	if got != want {
		t.Errorf("after upgrade goose version got=%d want=%d", got, want)
	}

	// And confirm the post-baseline migration's effect actually
	// landed: 00002 promotes the multipart indexes to partial UNIQUE.
	var idxSQL string
	row := sqlDB.QueryRowContext(context.Background(),
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_mpu_file_id_unique'`)
	if err := row.Scan(&idxSQL); err != nil {
		t.Fatalf("expected idx_mpu_file_id_unique to exist after upgrade: %v", err)
	}
	if !strings.Contains(strings.ToUpper(idxSQL), "UNIQUE") {
		t.Errorf("post-upgrade index isn't unique: %q", idxSQL)
	}
}

// TestOpenIsIdempotentAfterMigrations re-Opens an already-migrated DB
// and verifies goose doesn't re-run anything.
func TestOpenIsIdempotentAfterMigrations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.db")
	for i := 0; i < 3; i++ {
		d, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		_ = d.Close()
	}
	// Sanity: the version should still be the highest embedded.
	d, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("final Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	sqlDB, _ := d.g.DB()
	got, _ := goose.GetDBVersionContext(context.Background(), sqlDB)
	if got != highestEmbeddedMigrationVersion(t) {
		t.Errorf("version drifted across re-opens: %d", got)
	}
}

func loadMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatalf("read embedded migration %s: %v", name, err)
	}
	return string(b)
}

func highestEmbeddedMigrationVersion(t *testing.T) int64 {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var max int64
	for _, e := range entries {
		// goose filenames are NNNNN_name.sql.
		var v int64
		if _, err := parseLeadingInt(e.Name(), &v); err == nil && v > max {
			max = v
		}
	}
	if max == 0 {
		t.Fatal("no embedded migrations found")
	}
	return max
}

func parseLeadingInt(name string, out *int64) (int, error) {
	var v int64
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		v = v*10 + int64(name[i]-'0')
		i++
	}
	if i == 0 {
		return 0, sqlErrEmpty
	}
	*out = v
	return i, nil
}

var sqlErrEmpty = sql.ErrNoRows // any non-nil sentinel
