package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestMultipartUniqueIndexes verifies that the partial unique indexes
// added in #173 reject a second row with the same discriminator. Without
// them, Upsert's delete-then-insert is the only thing keeping the
// invariant — a crash window or a future concurrent path could leave
// two rows and GetMultipartUpload* would return an arbitrary one.
func TestMultipartUniqueIndexes(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()

	// Insert a row by hand (bypassing UpsertMultipartUpload, which
	// would delete first). Then a second row with the same file_id
	// should be rejected by the partial unique index.
	first := MultipartUpload{
		FileID:        42,
		S3Key:         "k",
		UploadID:      "u1",
		PartSize:      8 << 20,
		Size:          1 << 30,
		ContentSHA256: "x",
		StartedAt:     now,
		LastActiveAt:  now,
	}
	if err := d.g.WithContext(ctx).Create(&first).Error; err != nil {
		t.Fatalf("first insert: %v", err)
	}
	second := first
	second.ID = 0
	second.UploadID = "u2"
	err := d.g.WithContext(ctx).Create(&second).Error
	if err == nil {
		t.Fatalf("expected unique-violation on duplicate file_id, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("expected UNIQUE constraint error, got: %v", err)
	}

	// Same for zip_key.
	zipRow := MultipartUpload{
		ZipKey:        "photos/photos_1.zip",
		S3Key:         "k",
		UploadID:      "u1",
		PartSize:      8 << 20,
		Size:          1 << 30,
		ContentSHA256: "x",
		StartedAt:     now,
		LastActiveAt:  now,
	}
	if err := d.g.WithContext(ctx).Create(&zipRow).Error; err != nil {
		t.Fatalf("zip insert: %v", err)
	}
	zipDup := zipRow
	zipDup.ID = 0
	zipDup.UploadID = "u2"
	err = d.g.WithContext(ctx).Create(&zipDup).Error
	if err == nil {
		t.Fatalf("expected unique-violation on duplicate zip_key, got nil")
	}

	// Sanity: partial index does NOT reject the (file_id=0, zip_key="")
	// shape (which shouldn't happen in practice but the partial WHERE
	// must be permissive about it).
	bothEmpty := MultipartUpload{
		S3Key:         "k",
		UploadID:      "u1",
		PartSize:      8 << 20,
		Size:          1 << 30,
		ContentSHA256: "x",
		StartedAt:     now,
		LastActiveAt:  now,
	}
	if err := d.g.WithContext(ctx).Create(&bothEmpty).Error; err != nil {
		t.Fatalf("first empty-discriminator insert: %v", err)
	}
	bothEmpty.ID = 0
	if err := d.g.WithContext(ctx).Create(&bothEmpty).Error; err != nil {
		t.Errorf("second empty-discriminator insert should succeed (partial WHERE excludes them): %v", err)
	}
}

// TestUpsertMultipartUploadReplaces verifies that UpsertMultipartUpload
// continues to work after the partial unique indexes — the in-tx
// delete-then-create still satisfies the constraint because the old
// row is gone before the new one lands.
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
		t.Errorf("UploadID=%q want %q (latest write wins)", got.UploadID, "u2")
	}
}
