package db

import (
	"context"
	"testing"
	"time"
)

func TestRestoreLifecycle(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	seen := time.Now().UTC().Truncate(time.Second)
	if _, err := d.UpsertFile(ctx, "a.bin", 100, mtime, seen); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := d.MarkUploaded(ctx, 1, "md5", "backups/a.bin", seen); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}

	n, err := d.MarkRestoreInProgress(ctx, "backups/a.bin")
	if err != nil || n != 1 {
		t.Fatalf("MarkRestoreInProgress: n=%d err=%v", n, err)
	}
	files, _, err := d.ListFiles(ctx, FilesFilter{Search: "a.bin", All: true})
	if err != nil || len(files) != 1 {
		t.Fatalf("list: %v %d", err, len(files))
	}
	if files[0].RestoreStatus != RestoreStatusInProgress {
		t.Errorf("status = %q", files[0].RestoreStatus)
	}

	expires := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	if n, err := d.MarkRestored(ctx, "backups/a.bin", expires); err != nil || n != 1 {
		t.Fatalf("MarkRestored: n=%d err=%v", n, err)
	}
	files, _, _ = d.ListFiles(ctx, FilesFilter{Search: "a.bin", All: true})
	if files[0].RestoreStatus != RestoreStatusRestored {
		t.Errorf("status = %q", files[0].RestoreStatus)
	}
	if files[0].RestoreExpiresAt == nil || !files[0].RestoreExpiresAt.Equal(expires) {
		t.Errorf("expires = %v", files[0].RestoreExpiresAt)
	}

	// A late-arriving Post must not regress a row already restored.
	if _, err := d.MarkRestoreInProgress(ctx, "backups/a.bin"); err != nil {
		t.Fatalf("late post: %v", err)
	}
	files, _, _ = d.ListFiles(ctx, FilesFilter{Search: "a.bin", All: true})
	if files[0].RestoreStatus != RestoreStatusRestored {
		t.Errorf("regressed to %q", files[0].RestoreStatus)
	}

	// Unknown s3_key matches no rows; not an error.
	if n, err := d.MarkRestoreInProgress(ctx, "backups/nope"); err != nil || n != 0 {
		t.Errorf("unknown key: n=%d err=%v", n, err)
	}
}
