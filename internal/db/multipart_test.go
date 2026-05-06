package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestMultipartUniqueIndexes verifies that migration 00002's partial
// UNIQUE indexes reject a second row with the same discriminator.
// Without them, only `UpsertMultipartUpload`'s in-tx delete-then-create
// kept rows from duplicating — a crash window or a future concurrent
// path could leave two rows and `GetMultipartUploadByFile/ZipKey`'s
// `First()` would return an arbitrary one. (#173)
func TestMultipartUniqueIndexes(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()

	// Insert directly via GORM — bypasses Upsert's pre-delete so the
	// partial UNIQUE actually has to enforce the invariant.
	first := MultipartUpload{
		FileID: 42, S3Key: "k", UploadID: "u1",
		PartSize: 8 << 20, Size: 1 << 30, ContentSHA256: "x",
		StartedAt: now, LastActiveAt: now,
	}
	if err := d.g.WithContext(ctx).Create(&first).Error; err != nil {
		t.Fatalf("first insert: %v", err)
	}
	dup := first
	dup.ID = 0
	dup.UploadID = "u2"
	if err := d.g.WithContext(ctx).Create(&dup).Error; err == nil {
		t.Fatalf("expected UNIQUE violation on duplicate file_id, got nil")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("expected UNIQUE constraint error, got: %v", err)
	}

	// Same for zip_key.
	zip1 := MultipartUpload{
		ZipKey: "photos/photos_1.zip", S3Key: "k", UploadID: "u1",
		PartSize: 8 << 20, Size: 1 << 30, ContentSHA256: "x",
		StartedAt: now, LastActiveAt: now,
	}
	if err := d.g.WithContext(ctx).Create(&zip1).Error; err != nil {
		t.Fatalf("zip insert: %v", err)
	}
	zip2 := zip1
	zip2.ID = 0
	zip2.UploadID = "u2"
	if err := d.g.WithContext(ctx).Create(&zip2).Error; err == nil {
		t.Fatalf("expected UNIQUE violation on duplicate zip_key, got nil")
	}

	// The partial WHERE excludes the empty-discriminator shape (which
	// shouldn't happen in practice but the partial index must be
	// permissive about it).
	empty := MultipartUpload{
		S3Key: "k", UploadID: "u1",
		PartSize: 8 << 20, Size: 1 << 30, ContentSHA256: "x",
		StartedAt: now, LastActiveAt: now,
	}
	if err := d.g.WithContext(ctx).Create(&empty).Error; err != nil {
		t.Fatalf("first empty-discriminator insert: %v", err)
	}
	empty.ID = 0
	if err := d.g.WithContext(ctx).Create(&empty).Error; err != nil {
		t.Errorf("second empty-discriminator insert should succeed (partial WHERE excludes it): %v", err)
	}
}

// TestUpsertMultipartUploadReplaces verifies the existing upsert
// continues to work through the partial UNIQUE — the in-tx delete
// removes the old row before the new one lands.
func TestUpsertMultipartUploadReplaces(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()

	first := MultipartUpload{
		FileID: 7, S3Key: "k", UploadID: "u1", PartSize: 1 << 20,
		Size: 1 << 30, ContentSHA256: "x",
		StartedAt: now, LastActiveAt: now,
	}
	if err := d.UpsertMultipartUpload(ctx, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	second := first
	second.ID = 0
	second.UploadID = "u2"
	if err := d.UpsertMultipartUpload(ctx, second); err != nil {
		t.Fatalf("second upsert (replace): %v", err)
	}
	got, err := d.GetMultipartUploadByFile(ctx, 7)
	if err != nil || got == nil {
		t.Fatalf("get: row=%v err=%v", got, err)
	}
	if got.UploadID != "u2" {
		t.Errorf("UploadID=%q want u2 (latest write wins)", got.UploadID)
	}
}
