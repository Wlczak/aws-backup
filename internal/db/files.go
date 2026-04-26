package db

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// File statuses — authoritative list.
const (
	StatusPending  = "pending"
	StatusZipped   = "zipped"
	StatusUploaded = "uploaded"
	StatusFailed   = "failed"
	StatusMissing  = "missing"
)

// File is the GORM model for the `files` table.
type File struct {
	ID         int64     `gorm:"column:id;primaryKey;autoIncrement"`
	Path       string    `gorm:"column:path;uniqueIndex;not null"`
	Size       int64     `gorm:"column:size;not null"`
	MTime      time.Time `gorm:"column:mtime;not null"`
	MD5        string    `gorm:"column:md5"`
	Status     string    `gorm:"column:status;not null;default:'pending';index"`
	ZipName    string    `gorm:"column:zip_name;index"`
	S3Key      string    `gorm:"column:s3_key"`
	UploadedAt time.Time `gorm:"column:uploaded_at"`
	LastSeenAt time.Time `gorm:"column:last_seen_at;not null;index"`
}

// UpsertResult captures what changed during UpsertFile.
type UpsertResult struct {
	ID      int64
	Created bool
	Changed bool
}

// BatchEntry is a single file record passed to UpsertFileBatch.
type BatchEntry struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// UpsertFileBatch processes many entries in a single transaction.
// Returns one UpsertResult per entry in the same order.
func (db *DB) UpsertFileBatch(ctx context.Context, entries []BatchEntry, seenAt time.Time) ([]UpsertResult, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	results := make([]UpsertResult, len(entries))
	err := db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i, e := range entries {
			var existing File
			err := tx.Select("id, size, mtime").Where("path = ?", e.Path).First(&existing).Error
			if err != nil && err != gorm.ErrRecordNotFound {
				return err
			}
			if err == gorm.ErrRecordNotFound {
				f := File{Path: e.Path, Size: e.Size, MTime: e.ModTime, Status: StatusPending, LastSeenAt: seenAt}
				if err := tx.Omit("UploadedAt").Create(&f).Error; err != nil {
					return err
				}
				results[i] = UpsertResult{ID: f.ID, Created: true, Changed: true}
				continue
			}
			results[i].ID = existing.ID
			if e.Size != existing.Size || !e.ModTime.Equal(existing.MTime) {
				results[i].Changed = true
				// Preserve md5/zip_name/s3_key/uploaded_at as the historical
				// record of the *previous* uploaded version. status=pending
				// drives the new upload; the stale columns let
				// reconcileFromS3 distinguish "fresh row never uploaded"
				// (uploaded_at IS NULL → safe to bind to S3 zip) from
				// "modified-after-upload" (uploaded_at set → must NOT be
				// rebound to the old zip, or the new bytes are lost). See
				// #103.
				if err := tx.Model(&File{}).Where("id = ?", existing.ID).Updates(map[string]any{
					"size":         e.Size,
					"mtime":        e.ModTime,
					"status":       StatusPending,
					"last_seen_at": seenAt,
				}).Error; err != nil {
					return err
				}
			} else {
				if err := tx.Model(&File{}).Where("id = ?", existing.ID).
					Update("last_seen_at", seenAt).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	return results, err
}

// UpsertFile inserts or updates the path row, returning what changed.
func (db *DB) UpsertFile(ctx context.Context, path string, size int64, mtime, seenAt time.Time) (UpsertResult, error) {
	var res UpsertResult
	err := db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing File
		err := tx.Select("id, size, mtime").Where("path = ?", path).First(&existing).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return err
		}
		if err == gorm.ErrRecordNotFound {
			f := File{Path: path, Size: size, MTime: mtime, Status: StatusPending, LastSeenAt: seenAt}
			if err := tx.Omit("UploadedAt").Create(&f).Error; err != nil {
				return err
			}
			res = UpsertResult{ID: f.ID, Created: true, Changed: true}
			return nil
		}
		res.ID = existing.ID
		if size != existing.Size || !mtime.Equal(existing.MTime) {
			res.Changed = true
			// See UpsertFileBatch for why md5/zip_name/s3_key/uploaded_at
			// are preserved instead of cleared. (#103)
			return tx.Model(&File{}).Where("id = ?", existing.ID).Updates(map[string]any{
				"size":         size,
				"mtime":        mtime,
				"status":       StatusPending,
				"last_seen_at": seenAt,
			}).Error
		}
		return tx.Model(&File{}).Where("id = ?", existing.ID).
			Update("last_seen_at", seenAt).Error
	})
	return res, err
}

// MarkMissing flips any non-missing row whose last_seen_at is older than
// scanStart to status=missing. Returns the affected row count.
// Includes pending/failed so a file that was queued but deleted before
// upload doesn't sit in the queue forever, re-failing every run.
func (db *DB) MarkMissing(ctx context.Context, scanStart time.Time) (int64, error) {
	result := db.g.WithContext(ctx).Model(&File{}).
		Where("status IN ? AND last_seen_at < ?",
			[]string{StatusUploaded, StatusZipped, StatusPending, StatusFailed},
			scanStart).
		Update("status", StatusMissing)
	return result.RowsAffected, result.Error
}

// MarkUploaded sets md5, s3_key, uploaded_at and flips status to uploaded.
func (db *DB) MarkUploaded(ctx context.Context, id int64, md5, s3Key string, uploadedAt time.Time) error {
	return db.g.WithContext(ctx).Model(&File{}).Where("id = ?", id).Updates(map[string]any{
		"md5":         md5,
		"s3_key":      s3Key,
		"uploaded_at": uploadedAt,
		"status":      StatusUploaded,
	}).Error
}

// sqlChunkSize is the maximum number of values in a single IN clause.
// SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 999; stay well under it.
const sqlChunkSize = 500

// MarkZipUploadedBatch attaches a zip_name and flips many ids straight to
// 'uploaded' (md5/s3_key/uploaded_at populated) in a single transaction.
// Used by the engine after a successful zip+sidecar upload so the rows
// don't sit in the intermediate 'zipped' state if the second of two
// updates fails.
func (db *DB) MarkZipUploadedBatch(ctx context.Context, ids []int64, zipName, md5, s3Key string, uploadedAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for len(ids) > 0 {
			chunk := ids
			if len(chunk) > sqlChunkSize {
				chunk = ids[:sqlChunkSize]
			}
			ids = ids[len(chunk):]
			if err := tx.Model(&File{}).Where("id IN ?", chunk).Updates(map[string]any{
				"zip_name":    zipName,
				"md5":         md5,
				"s3_key":      s3Key,
				"uploaded_at": uploadedAt,
				"status":      StatusUploaded,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// MarkUploadedBatch flips many ids to 'uploaded' sharing a single md5/s3_key.
// All chunks run inside a single transaction so a partial failure cannot
// leave half the batch uploaded and half stuck in the prior state. (#110)
func (db *DB) MarkUploadedBatch(ctx context.Context, ids []int64, md5, s3Key string, uploadedAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for len(ids) > 0 {
			chunk := ids
			if len(chunk) > sqlChunkSize {
				chunk = ids[:sqlChunkSize]
			}
			ids = ids[len(chunk):]
			if err := tx.Model(&File{}).Where("id IN ?", chunk).Updates(map[string]any{
				"md5":         md5,
				"s3_key":      s3Key,
				"uploaded_at": uploadedAt,
				"status":      StatusUploaded,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// UploadedRow describes one individual-upload outcome to flush in a batch.
type UploadedRow struct {
	ID         int64
	MD5        string
	S3Key      string
	UploadedAt time.Time
}

// MarkUploadedMany applies per-file uploaded state in a single transaction.
func (db *DB) MarkUploadedMany(ctx context.Context, rows []UploadedRow) error {
	if len(rows) == 0 {
		return nil
	}
	return db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, r := range rows {
			if err := tx.Model(&File{}).Where("id = ?", r.ID).Updates(map[string]any{
				"md5":         r.MD5,
				"s3_key":      r.S3Key,
				"uploaded_at": r.UploadedAt,
				"status":      StatusUploaded,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// MarkFailed sets the file status to failed.
func (db *DB) MarkFailed(ctx context.Context, id int64) error {
	return db.g.WithContext(ctx).Model(&File{}).Where("id = ?", id).
		Update("status", StatusFailed).Error
}

// SetZipName attaches a zip name to a batch of file IDs and flips status to zipped.
// Wrapped in a single transaction so a partial chunk failure cannot leave
// the batch split across statuses. (#110)
func (db *DB) SetZipName(ctx context.Context, ids []int64, zipName string) error {
	if len(ids) == 0 {
		return nil
	}
	return db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for len(ids) > 0 {
			chunk := ids
			if len(chunk) > sqlChunkSize {
				chunk = ids[:sqlChunkSize]
			}
			ids = ids[len(chunk):]
			if err := tx.Model(&File{}).Where("id IN ?", chunk).Updates(map[string]any{
				"zip_name": zipName,
				"status":   StatusZipped,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// FilesFilter describes a ListFiles query.
type FilesFilter struct {
	Status string
	Search string
	Page   int
	Limit  int
	All    bool
}

// ListFiles returns a page of file rows matching filter, plus the total row count.
func (db *DB) ListFiles(ctx context.Context, f FilesFilter) ([]File, int64, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Page <= 0 {
		f.Page = 1
	}

	buildQ := func() *gorm.DB {
		q := db.g.WithContext(ctx).Model(&File{})
		if f.Status != "" {
			q = q.Where("status = ?", f.Status)
		}
		if f.Search != "" {
			q = q.Where("path LIKE ?", "%"+f.Search+"%")
		}
		return q
	}

	var total int64
	if err := buildQ().Count(&total).Error; err != nil {
		return nil, 0, err
	}

	q := buildQ().Order("path")
	if !f.All {
		q = q.Offset((f.Page - 1) * f.Limit).Limit(f.Limit)
	}
	var files []File
	if err := q.Find(&files).Error; err != nil {
		return nil, 0, err
	}
	return files, total, nil
}

// FileStats is the aggregate view for the dashboard.
type FileStats struct {
	ByStatus   map[string]int64
	TotalSize  int64
	TotalCount int64
}

// Stats returns per-status counts plus total size/count across the index.
func (db *DB) Stats(ctx context.Context) (FileStats, error) {
	s := FileStats{ByStatus: map[string]int64{}}

	var rows []struct {
		Status string
		Count  int64
	}
	if err := db.g.WithContext(ctx).Model(&File{}).
		Select("status, COUNT(*) as count").
		Group("status").
		Scan(&rows).Error; err != nil {
		return s, err
	}
	for _, r := range rows {
		s.ByStatus[r.Status] = r.Count
		s.TotalCount += r.Count
	}

	if err := db.g.WithContext(ctx).Model(&File{}).
		Select("COALESCE(SUM(size), 0)").
		Scan(&s.TotalSize).Error; err != nil {
		return s, err
	}
	return s, nil
}

// MarkPendingByIDs resets md5/zip_name/s3_key/uploaded_at and flips status to pending.
// All chunks run inside a single transaction so a partial failure cannot
// leave the batch split across statuses. (#110)
func (db *DB) MarkPendingByIDs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var total int64
	err := db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for len(ids) > 0 {
			chunk := ids
			if len(chunk) > sqlChunkSize {
				chunk = ids[:sqlChunkSize]
			}
			ids = ids[len(chunk):]
			result := tx.Model(&File{}).Where("id IN ?", chunk).Updates(map[string]any{
				"status":      StatusPending,
				"md5":         gorm.Expr("NULL"),
				"zip_name":    gorm.Expr("NULL"),
				"s3_key":      gorm.Expr("NULL"),
				"uploaded_at": gorm.Expr("NULL"),
			})
			if result.Error != nil {
				return result.Error
			}
			total += result.RowsAffected
		}
		return nil
	})
	return total, err
}

// PurgeMissingFiles deletes every row whose status is 'missing'.
func (db *DB) PurgeMissingFiles(ctx context.Context) (int64, error) {
	result := db.g.WithContext(ctx).Where("status = ?", StatusMissing).Delete(&File{})
	return result.RowsAffected, result.Error
}

// MarkAllFailedPending is the bulk 'retry everything that failed' path.
func (db *DB) MarkAllFailedPending(ctx context.Context) (int64, error) {
	result := db.g.WithContext(ctx).Model(&File{}).Where("status = ?", StatusFailed).Updates(map[string]any{
		"status":      StatusPending,
		"md5":         gorm.Expr("NULL"),
		"zip_name":    gorm.Expr("NULL"),
		"s3_key":      gorm.Expr("NULL"),
		"uploaded_at": gorm.Expr("NULL"),
	})
	return result.RowsAffected, result.Error
}

// DeleteFiles removes the given rows from the files table.
// All chunks run inside a single transaction so a partial failure cannot
// leave half the batch deleted. (#110)
func (db *DB) DeleteFiles(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var total int64
	err := db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for len(ids) > 0 {
			chunk := ids
			if len(chunk) > sqlChunkSize {
				chunk = ids[:sqlChunkSize]
			}
			ids = ids[len(chunk):]
			result := tx.Where("id IN ?", chunk).Delete(&File{})
			if result.Error != nil {
				return result.Error
			}
			total += result.RowsAffected
		}
		return nil
	})
	return total, err
}

// ListPending returns every row the engine should attempt to upload.
func (db *DB) ListPending(ctx context.Context, includeFailed bool) ([]File, error) {
	statuses := []string{StatusPending}
	if includeFailed {
		statuses = append(statuses, StatusFailed)
	}
	var files []File
	err := db.g.WithContext(ctx).Where("status IN ?", statuses).Order("path").Find(&files).Error
	return files, err
}

// ListZipNames returns every distinct non-empty zip_name in the index.
func (db *DB) ListZipNames(ctx context.Context) ([]string, error) {
	var names []string
	err := db.g.WithContext(ctx).Model(&File{}).
		Where("zip_name != ''").
		Distinct("zip_name").
		Order("zip_name").
		Pluck("zip_name", &names).Error
	return names, err
}

// ListIndividualS3Keys returns distinct s3_key values for individually-uploaded files.
func (db *DB) ListIndividualS3Keys(ctx context.Context) ([]string, error) {
	var keys []string
	err := db.g.WithContext(ctx).Model(&File{}).
		Where("COALESCE(zip_name,'') = '' AND COALESCE(s3_key,'') != ''").
		Distinct("s3_key").
		Order("s3_key").
		Pluck("s3_key", &keys).Error
	return keys, err
}

// ReconcileZip marks files as uploaded based on an S3 index sidecar.
//
// The match is restricted to rows that look genuinely orphaned in the
// crash window between a successful S3 zip upload and its DB commit:
// either status='zipped' (the intended intermediate state, now atomic
// per #84) or pending/failed rows that still have NULL md5 / zip_name /
// uploaded_at. A pending row that already carries those columns means
// UpsertFileBatch saw a size/mtime change after the previous upload and
// cleared them — re-binding it to the old zip would silently lose the
// modified content (the new bytes never get uploaded). See #103.
func (db *DB) ReconcileZip(ctx context.Context, paths []string, zipRel, s3Key string, now time.Time) (int64, error) {
	var total int64
	for len(paths) > 0 {
		chunk := paths
		if len(chunk) > sqlChunkSize {
			chunk = paths[:sqlChunkSize]
		}
		paths = paths[len(chunk):]
		// Match either:
		//   - status='zipped' (the legacy intermediate state, kept for
		//     migrations from older deployments), OR
		//   - status in (pending, failed) AND uploaded_at IS NULL — i.e.
		//     a row that has never been uploaded. This guards against
		//     re-binding a freshly-modified file (whose row was reset to
		//     pending by UpsertFile but retains uploaded_at from the
		//     previous version) back to its stale zip and silently losing
		//     the new content. See #103.
		result := db.g.WithContext(ctx).Model(&File{}).
			Where("path IN ? AND ("+
				"status = ? OR "+
				"(status IN ? AND uploaded_at IS NULL)"+
				")",
				chunk,
				StatusZipped,
				[]string{StatusPending, StatusFailed},
			).
			Updates(map[string]any{
				"zip_name":    zipRel,
				"s3_key":      s3Key,
				"uploaded_at": now,
				"status":      StatusUploaded,
			})
		if result.Error != nil {
			return total, result.Error
		}
		total += result.RowsAffected
	}
	return total, nil
}

// MarkPendingByPaths resets files to pending by exact path match.
func (db *DB) MarkPendingByPaths(ctx context.Context, paths []string) (int64, error) {
	return markPendingByColumn(ctx, db.g, "path", paths)
}

// MarkPendingByZipNames resets all files whose zip_name is in the list.
func (db *DB) MarkPendingByZipNames(ctx context.Context, zipNames []string) (int64, error) {
	return markPendingByColumn(ctx, db.g, "zip_name", zipNames)
}

// MarkPendingByS3Keys resets individually-uploaded files whose s3_key is in the list.
func (db *DB) MarkPendingByS3Keys(ctx context.Context, keys []string) (int64, error) {
	return markPendingByColumn(ctx, db.g, "s3_key", keys)
}

// markPendingByColumn is the shared implementation for the three MarkPendingBy* functions.
// column must be one of the validated constants: "path", "zip_name", "s3_key".
// All chunks run inside a single transaction so a partial failure cannot
// leave the batch split across statuses. (#110)
func markPendingByColumn(ctx context.Context, g *gorm.DB, column string, values []string) (int64, error) {
	if len(values) == 0 {
		return 0, nil
	}
	var total int64
	err := g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for len(values) > 0 {
			chunk := values
			if len(chunk) > sqlChunkSize {
				chunk = values[:sqlChunkSize]
			}
			values = values[len(chunk):]
			result := tx.Model(&File{}).
				Where(fmt.Sprintf("status != ? AND %s IN ?", column), StatusMissing, chunk).
				Updates(map[string]any{
					"status":      StatusPending,
					"md5":         gorm.Expr("NULL"),
					"zip_name":    gorm.Expr("NULL"),
					"s3_key":      gorm.Expr("NULL"),
					"uploaded_at": gorm.Expr("NULL"),
				})
			if result.Error != nil {
				return result.Error
			}
			total += result.RowsAffected
		}
		return nil
	})
	return total, err
}

