package db

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// MultipartUpload tracks an in-flight S3 multipart upload so the next
// run can resume from the parts already accepted instead of starting
// from byte 0. Exactly one of FileID / ZipKey is populated per row:
//
//   - FileID > 0, ZipKey "" — individual file upload
//   - FileID = 0, ZipKey != "" — zip group upload (key is the
//     repo-relative zip name, matching files.zip_name)
//
// Rows are deleted on successful CompleteMultipartUpload (engine-side)
// and on aborted-by-checksum-mismatch (storage-side). A row that
// outlives the local tmp file is harmless — it just gets discarded the
// next time the engine sees the local file is gone or differs.
// The `uniqueIndex` tags below carry partial-WHERE clauses so the
// natural-key invariant (one in-flight upload per file or per zip) is
// enforced at the schema level — without them, only application-level
// discipline in UpsertMultipartUpload's delete-then-create kept rows
// from duplicating, and a crash window or a future concurrent path
// could leave two rows with the same discriminator. (#173)
type MultipartUpload struct {
	ID            int64     `gorm:"column:id;primaryKey;autoIncrement"`
	FileID        int64     `gorm:"column:file_id;uniqueIndex:idx_mpu_file_id_unique,where:file_id > 0"`
	ZipKey        string    `gorm:"column:zip_key;uniqueIndex:idx_mpu_zip_key_unique,where:zip_key != ''"`
	S3Key         string    `gorm:"column:s3_key;not null"`
	UploadID      string    `gorm:"column:upload_id;not null"`
	PartSize      int64     `gorm:"column:part_size;not null"`
	Size          int64     `gorm:"column:size;not null"`
	ContentSHA256 string    `gorm:"column:content_sha256;not null"`
	StartedAt     time.Time `gorm:"column:started_at;not null"`
	LastActiveAt  time.Time `gorm:"column:last_active_at;not null"`
}

// TableName pins the table name (GORM would otherwise pluralise to
// `multipart_uploads` anyway, but explicit is cheap and stable across
// gorm versions).
func (MultipartUpload) TableName() string { return "multipart_uploads" }

// GetMultipartUploadByFile returns the row for an individual file's
// in-flight upload, or (nil, nil) when there is none. The caller
// branches on nil to decide between fresh CreateMultipartUpload and
// ListParts-based resume.
func (db *DB) GetMultipartUploadByFile(ctx context.Context, fileID int64) (*MultipartUpload, error) {
	if fileID <= 0 {
		return nil, nil
	}
	var mu MultipartUpload
	err := db.g.WithContext(ctx).Where("file_id = ?", fileID).First(&mu).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mu, nil
}

// GetMultipartUploadByZipKey returns the row for a zip's in-flight
// upload, or (nil, nil) when there is none.
func (db *DB) GetMultipartUploadByZipKey(ctx context.Context, zipKey string) (*MultipartUpload, error) {
	if zipKey == "" {
		return nil, nil
	}
	var mu MultipartUpload
	err := db.g.WithContext(ctx).Where("zip_key = ?", zipKey).First(&mu).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mu, nil
}

// UpsertMultipartUpload writes or updates a multipart-upload record.
// The (file_id, zip_key) discriminator is used as the natural key —
// only one in-flight upload can exist per file or per zip at a time,
// so a duplicate insert means the previous run's UploadId was
// abandoned and is being replaced (the storage layer aborts the old
// one before this is called).
func (db *DB) UpsertMultipartUpload(ctx context.Context, mu MultipartUpload) error {
	now := time.Now().UTC()
	if mu.StartedAt.IsZero() {
		mu.StartedAt = now
	}
	mu.LastActiveAt = now
	return db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if mu.FileID > 0 {
			if err := tx.Where("file_id = ?", mu.FileID).Delete(&MultipartUpload{}).Error; err != nil {
				return err
			}
		} else if mu.ZipKey != "" {
			if err := tx.Where("zip_key = ?", mu.ZipKey).Delete(&MultipartUpload{}).Error; err != nil {
				return err
			}
		}
		return tx.Create(&mu).Error
	})
}

// TouchMultipartUpload bumps last_active_at so the optional
// "older than N days" sweep doesn't reap an upload that's actively
// being resumed.
func (db *DB) TouchMultipartUpload(ctx context.Context, id int64) error {
	return db.g.WithContext(ctx).Model(&MultipartUpload{}).
		Where("id = ?", id).
		Update("last_active_at", time.Now().UTC()).Error
}

// DeleteMultipartUploadByFile removes the row for a file's upload.
// Called on success or after AbortMultipartUpload.
func (db *DB) DeleteMultipartUploadByFile(ctx context.Context, fileID int64) error {
	if fileID <= 0 {
		return nil
	}
	return db.g.WithContext(ctx).Where("file_id = ?", fileID).Delete(&MultipartUpload{}).Error
}

// DeleteMultipartUploadByZipKey removes the row for a zip's upload.
func (db *DB) DeleteMultipartUploadByZipKey(ctx context.Context, zipKey string) error {
	if zipKey == "" {
		return nil
	}
	return db.g.WithContext(ctx).Where("zip_key = ?", zipKey).Delete(&MultipartUpload{}).Error
}

// ListStaleMultipartUploads returns rows whose last_active_at is
// older than `before`. Hook for a future "abort uploads we've stopped
// touching" sweep. Not wired in yet — the bucket lifecycle policy is
// the v1 cleanup mechanism per the design doc — but the helper
// belongs with the rest so it grows alongside the schema.
func (db *DB) ListStaleMultipartUploads(ctx context.Context, before time.Time) ([]MultipartUpload, error) {
	var out []MultipartUpload
	err := db.g.WithContext(ctx).Where("last_active_at < ?", before).Find(&out).Error
	return out, err
}
