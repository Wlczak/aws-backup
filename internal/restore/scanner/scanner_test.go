package scanner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/events"
	"github.com/Wlczak/aws-backup/internal/storage"
)

// TestRunFullIncludesZipBackedObjects verifies that the authoritative
// restore scan heads every uploaded object key, not just standalone
// uploads. Zip-backed rows must be included or full scans report 0
// HEADs even when the bucket is populated.
func TestRunFullIncludesZipBackedObjects(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	store := storage.NewMemStorage()
	bus := events.NewBus(1)
	sc := New(d, func() storage.Storage { return store }, bus, nil)

	now := time.Now().UTC()

	standalone, err := d.UpsertFile(ctx, "solo.txt", 10, now, now)
	if err != nil {
		t.Fatalf("seed standalone: %v", err)
	}
	if err := d.MarkUploaded(ctx, standalone.ID, "md5", "backups/solo.txt", now); err != nil {
		t.Fatalf("mark standalone uploaded: %v", err)
	}
	if _, err := store.Put(ctx, "backups/solo.txt", strings.NewReader("solo"), 4); err != nil {
		t.Fatalf("put standalone object: %v", err)
	}
	if _, err := d.MarkRestoreInProgress(ctx, "backups/solo.txt"); err != nil {
		t.Fatalf("mark standalone in progress: %v", err)
	}

	zipped, err := d.UpsertFile(ctx, "docs/spec.pdf", 20, now, now)
	if err != nil {
		t.Fatalf("seed zipped: %v", err)
	}
	if err := d.SetZipName(ctx, []int64{zipped.ID}, "docs/docs_1.zip"); err != nil {
		t.Fatalf("set zip name: %v", err)
	}
	if err := d.MarkUploaded(ctx, zipped.ID, "md5", "backups/docs/docs_1.zip", now); err != nil {
		t.Fatalf("mark zipped uploaded: %v", err)
	}
	if _, err := store.Put(ctx, "backups/docs/docs_1.zip", strings.NewReader("zip"), 3); err != nil {
		t.Fatalf("put zip object: %v", err)
	}
	if _, err := d.MarkRestoreInProgress(ctx, "backups/docs/docs_1.zip"); err != nil {
		t.Fatalf("mark zip in progress: %v", err)
	}

	res, err := sc.RunFull(ctx)
	if err != nil {
		t.Fatalf("RunFull: %v", err)
	}
	if res.Scanned != 2 || res.Updated != 2 || res.Errors != 0 {
		t.Fatalf("RunFull result = %+v, want scanned=2 updated=2 errors=0", res)
	}
}
