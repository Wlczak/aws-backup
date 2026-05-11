package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"
)

// flexTimeLayouts is the list of textual timestamp formats the
// `glebarez/sqlite` driver might hand back for a TIMESTAMP column.
// Order is significant: the canonical Go-default layout (the one GORM
// writes today) is checked first so the common path is one Parse.
//
// Old DBs created before goose migrations were introduced may carry
// rows in any of these formats — `time.Time.String()`-style with
// `MST`, RFC3339 with or without nanos, the SQLite-default
// `YYYY-MM-DD HH:MM:SS`, etc. SQLite stores TIMESTAMP as TEXT and
// makes no attempt to canonicalise; aggregates like `MIN(...)` then
// surface whichever stored string sorts smallest.
var flexTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999 -0700 MST",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseFlexTime tries every known layout against s. Returns
// (zero, false) if every layout fails. Callers treat that as
// "skip this row" rather than fatal — a single corrupted timestamp
// shouldn't 500 the whole dashboard.
func parseFlexTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range flexTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// likeEscape escapes the LIKE meta-characters % and _ (plus the escape
// character itself) in a user-supplied search term so a query like
// "?search=%" doesn't return every row, and paths containing literal
// % or _ match correctly. (#67)
var likeEscape = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// File statuses — authoritative list.
const (
	StatusPending  = "pending"
	StatusZipped   = "zipped"
	StatusUploaded = "uploaded"
	StatusFailed   = "failed"
	StatusMissing  = "missing"
)

// Restore lifecycle states tracked separately from Status. Empty string =
// no known restore activity for the row.
const (
	RestoreStatusInProgress = "in_progress"
	RestoreStatusRestored   = "restored"
)

// File is the GORM model for the `files` table.
type File struct {
	ID            int64     `gorm:"column:id;primaryKey;autoIncrement"`
	Path          string    `gorm:"column:path;uniqueIndex;not null"`
	Size          int64     `gorm:"column:size;not null"`
	MTime         time.Time `gorm:"column:mtime;not null"`
	MD5           string    `gorm:"column:md5"`
	Status        string    `gorm:"column:status;not null;default:'pending';index"`
	ZipName       string    `gorm:"column:zip_name;index"`
	S3Key         string    `gorm:"column:s3_key"`
	UploadedAt    time.Time `gorm:"column:uploaded_at"`
	LastSeenAt    time.Time `gorm:"column:last_seen_at;not null;index"`
	RestoreStatus string    `gorm:"column:restore_status;index"`
	// restoreExpiresAtRaw is the untyped TEXT we read out of SQLite —
	// the column has documented format drift (RFC3339 vs. Go-default vs.
	// SQLite-default) that breaks glebarez/sqlite's single-layout Scan
	// and used to 500 every Files page that included a legacy row.
	// AfterFind below flex-parses it into RestoreExpiresAt so a single
	// corrupted timestamp produces a nil pointer + log, not a fatal
	// row Scan error. (#248)
	RestoreExpiresAtRaw sql.NullString `gorm:"column:restore_expires_at" json:"-"`
	// RestoreExpiresAt is parsed from the raw column in AfterFind;
	// callers continue to read it as the typed *time.Time they always did.
	RestoreExpiresAt *time.Time `gorm:"-" json:"restore_expires_at,omitempty"`
}

// AfterFind parses restore_expires_at via parseFlexTime so legacy /
// non-canonical layouts don't poison the whole page. (#248)
func (f *File) AfterFind(_ *gorm.DB) error {
	if !f.RestoreExpiresAtRaw.Valid || strings.TrimSpace(f.RestoreExpiresAtRaw.String) == "" {
		f.RestoreExpiresAt = nil
		return nil
	}
	if t, ok := parseFlexTime(f.RestoreExpiresAtRaw.String); ok {
		t = t.UTC()
		f.RestoreExpiresAt = &t
	} else {
		f.RestoreExpiresAt = nil
	}
	return nil
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
//
// Implementation: a re-scan over a stable tree is dominated by unchanged
// rows, so we partition entries via one bulk SELECT, then issue bulk
// INSERT (new) + bulk UPDATE last_seen_at (unchanged) + per-row UPDATE
// (changed). The previous version did SELECT-then-INSERT/UPDATE per
// entry which made a 100k-file scan take minutes of GORM round-trips.
// (#scan-batch)
func (db *DB) UpsertFileBatch(ctx context.Context, entries []BatchEntry, seenAt time.Time) ([]UpsertResult, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	results := make([]UpsertResult, len(entries))

	paths := make([]string, len(entries))
	indexByPath := make(map[string]int, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
		indexByPath[e.Path] = i
	}

	err := db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Bulk-fetch every existing row in chunks to keep IN-list size
		// under SQLite's variable cap.
		existing := make(map[string]File, len(entries))
		for s := 0; s < len(paths); s += sqlChunkSize {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := s + sqlChunkSize
			if end > len(paths) {
				end = len(paths)
			}
			var rows []File
			if err := tx.Select("id, path, size, mtime").
				Where("path IN ?", paths[s:end]).Find(&rows).Error; err != nil {
				return err
			}
			for _, r := range rows {
				existing[r.Path] = r
			}
		}

		// Partition entries into create / changed / unchanged buckets.
		var toCreate []File
		var toCreateIdx []int
		var unchangedIDs []int64
		type changeOp struct {
			id    int64
			size  int64
			mtime time.Time
		}
		var changes []changeOp

		for i, e := range entries {
			ex, ok := existing[e.Path]
			if !ok {
				toCreate = append(toCreate, File{
					Path: e.Path, Size: e.Size, MTime: e.ModTime,
					Status: StatusPending, LastSeenAt: seenAt,
				})
				toCreateIdx = append(toCreateIdx, i)
				continue
			}
			results[i].ID = ex.ID
			if e.Size != ex.Size || !e.ModTime.Equal(ex.MTime) {
				results[i].Changed = true
				changes = append(changes, changeOp{id: ex.ID, size: e.Size, mtime: e.ModTime})
			} else {
				unchangedIDs = append(unchangedIDs, ex.ID)
			}
		}

		// Bulk INSERT new rows. CreateInBatches mutates the slice with
		// the assigned auto-increment IDs as each batch commits.
		if len(toCreate) > 0 {
			if err := tx.Omit("UploadedAt").CreateInBatches(toCreate, sqlChunkSize).Error; err != nil {
				return err
			}
			for j, f := range toCreate {
				idx := toCreateIdx[j]
				results[idx] = UpsertResult{ID: f.ID, Created: true, Changed: true}
			}
		}

		// Bulk UPDATE last_seen_at on unchanged rows — one statement per
		// chunk instead of one per row.
		for s := 0; s < len(unchangedIDs); s += sqlChunkSize {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := s + sqlChunkSize
			if end > len(unchangedIDs) {
				end = len(unchangedIDs)
			}
			if err := tx.Model(&File{}).
				Where("id IN ?", unchangedIDs[s:end]).
				Update("last_seen_at", seenAt).Error; err != nil {
				return err
			}
		}

		// Changed rows still go one-by-one because each carries a
		// different (size, mtime) pair. Re-scans of stable trees rarely
		// touch this path, so the per-row cost is fine.
		// Preserve md5/zip_name/s3_key/uploaded_at as the historical
		// record of the *previous* uploaded version. status=pending
		// drives the new upload; the stale columns let reconcileFromS3
		// distinguish "fresh row never uploaded" (uploaded_at IS NULL →
		// safe to bind to S3 zip) from "modified-after-upload"
		// (uploaded_at set → must NOT be rebound to the old zip, or the
		// new bytes are lost). See #103.
		for _, c := range changes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := tx.Model(&File{}).Where("id = ?", c.id).Updates(map[string]any{
				"size":               c.size,
				"mtime":              c.mtime,
				"status":             StatusPending,
				"last_seen_at":       seenAt,
				"restore_status":     "",
				"restore_expires_at": nil,
			}).Error; err != nil {
				return err
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
				"size":               size,
				"mtime":              mtime,
				"status":             StatusPending,
				"last_seen_at":       seenAt,
				"restore_status":     "",
				"restore_expires_at": nil,
			}).Error
		}
		return tx.Model(&File{}).Where("id = ?", existing.ID).
			Update("last_seen_at", seenAt).Error
	})
	return res, err
}

// MarkMissingByPaths flips rows with the given source-relative paths to
// status=missing. Used after operator-driven cloud deletes so the next
// existence check doesn't observe the absence and re-queue the file
// for upload. Per the project index-semantics rule the row stays
// (missing rows track bucket state, not source state). (#132)
func (db *DB) MarkMissingByPaths(ctx context.Context, paths []string) (int64, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	var total int64
	err := db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for len(paths) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			chunk := paths
			if len(chunk) > sqlChunkSize {
				chunk = paths[:sqlChunkSize]
			}
			paths = paths[len(chunk):]
			res := tx.Model(&File{}).Where("path IN ?", chunk).
				Update("status", StatusMissing)
			if res.Error != nil {
				return res.Error
			}
			total += res.RowsAffected
		}
		return nil
	})
	return total, err
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
			if err := ctx.Err(); err != nil { // cancel-aware (#227)
				return err
			}
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
			if err := ctx.Err(); err != nil { // cancel-aware (#227)
				return err
			}
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
// Checks ctx between rows so a cancel doesn't keep holding the SQLite write
// lock through every remaining UPDATE. (#171)
func (db *DB) MarkUploadedMany(ctx context.Context, rows []UploadedRow) error {
	if len(rows) == 0 {
		return nil
	}
	return db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, r := range rows {
			if err := ctx.Err(); err != nil {
				return err
			}
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
			if err := ctx.Err(); err != nil { // cancel-aware (#227)
				return err
			}
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
			q = q.Where(`path LIKE ? ESCAPE '\'`, "%"+likeEscape.Replace(f.Search)+"%")
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

// TreeFolder is one immediate-child folder under a prefix in the lazy
// tree view. Aggregates roll up over the entire subtree so the row can
// render "12 files · 3.4 MB" without recursing.
type TreeFolder struct {
	Name      string
	Path      string
	FileCount int64
	TotalSize int64
}

// ListTreeChildren returns the immediate children (files + first-level
// subfolders) of `prefix` in the file index. Subfolders are aggregated
// over their full subtree so the caller can render counts without
// loading descendants. Use prefix == "" for the root level.
//
// The query scans the subtree under prefix once and partitions in Go,
// which is bounded by the size of that subtree (typically much smaller
// than the full index). Status filters narrow the scan; folder
// aggregates only count rows that pass the filter.
func (db *DB) ListTreeChildren(ctx context.Context, prefix, statusFilter string) ([]TreeFolder, []File, error) {
	q := db.g.WithContext(ctx).Model(&File{})
	if statusFilter != "" {
		q = q.Where("status = ?", statusFilter)
	}
	if prefix != "" {
		// LIKE 'prefix/%' uses the unique index on path for ASCII
		// prefixes and falls back to a sequential scan otherwise; either
		// way the rowset is the subtree, not the whole index.
		q = q.Where(`path LIKE ? ESCAPE '\'`, likeEscape.Replace(prefix)+"/%")
	}

	var rows []File
	if err := q.Order("path").Find(&rows).Error; err != nil {
		return nil, nil, err
	}

	cut := 0
	if prefix != "" {
		cut = len(prefix) + 1
	}

	folderByName := make(map[string]*TreeFolder)
	var folders []*TreeFolder
	var files []File
	for _, f := range rows {
		if cut > len(f.Path) {
			continue
		}
		rest := f.Path[cut:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			files = append(files, f)
			continue
		}
		name := rest[:slash]
		fp := folderByName[name]
		if fp == nil {
			fullPath := name
			if prefix != "" {
				fullPath = prefix + "/" + name
			}
			fp = &TreeFolder{Name: name, Path: fullPath}
			folderByName[name] = fp
			folders = append(folders, fp)
		}
		fp.FileCount++
		fp.TotalSize += f.Size
	}

	out := make([]TreeFolder, len(folders))
	for i, p := range folders {
		out[i] = *p
	}
	return out, files, nil
}

// ListSubtreeIDs returns the IDs and paths of every file under prefix
// (recursive). Used by the lazy tree view's folder-checkbox so a
// "select folder" click can fan out to all descendant files without
// the client first having to expand them. Capped at maxRows; callers
// should pass a sensible limit so a "select root" click can't OOM.
func (db *DB) ListSubtreeIDs(ctx context.Context, prefix, statusFilter string, maxRows int) ([]int64, []string, int64, error) {
	q := db.g.WithContext(ctx).Model(&File{})
	if statusFilter != "" {
		q = q.Where("status = ?", statusFilter)
	}
	if prefix != "" {
		q = q.Where(`path LIKE ? ESCAPE '\'`, likeEscape.Replace(prefix)+"/%")
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, nil, 0, err
	}
	if maxRows <= 0 {
		maxRows = 50000
	}
	q = q.Order("path").Limit(maxRows)
	var rows []File
	if err := q.Select("id", "path").Find(&rows).Error; err != nil {
		return nil, nil, 0, err
	}
	ids := make([]int64, len(rows))
	paths := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
		paths[i] = r.Path
	}
	return ids, paths, total, nil
}

// FileStats is the aggregate view for the dashboard.
type FileStats struct {
	ByStatus          map[string]int64
	ByRestoreStatus   map[string]int64
	RestoreSoonestExp *time.Time
	TotalSize         int64
	TotalCount        int64
}

// Stats returns per-status counts plus total size/count across the index.
func (db *DB) Stats(ctx context.Context) (FileStats, error) {
	s := FileStats{ByStatus: map[string]int64{}, ByRestoreStatus: map[string]int64{}}

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

	var rrows []struct {
		RestoreStatus string
		Count         int64
	}
	if err := db.g.WithContext(ctx).Model(&File{}).
		Select("restore_status, COUNT(*) as count").
		Where("restore_status != ''").
		Group("restore_status").
		Scan(&rrows).Error; err != nil {
		return s, err
	}
	for _, r := range rrows {
		s.ByRestoreStatus[r.RestoreStatus] = r.Count
	}

	// Soonest expiry across rows that are still in the restored window —
	// surfaces "your restored copies expire on …" on the dashboard.
	//
	// Scan into NullString rather than NullTime: the pure-Go
	// `glebarez/sqlite` driver hands back the raw TEXT for any row
	// whose stored format doesn't match its expected layout, and
	// `sql.NullTime.Scan` then fails with "string into *time.Time".
	// Pre-migrations DBs in this project carry mixed formats from
	// older code paths, so we scan defensively and parse with a list
	// of known layouts. If no layout matches we log and skip — a
	// single bad row shouldn't 500 the dashboard.
	// Compute MIN in Go: SQLite stores DATETIME as TEXT and the column
	// has documented mixed layouts. SQL `>` and MIN() over those rows
	// would compare lexically (e.g. "T" > " " in RFC3339 vs Go-default),
	// so the dashboard's "soonest expiring" could pick an already-expired
	// row or skip the actual soonest. Pull every restored row's raw
	// timestamp, flex-parse, filter > now, take MIN. (#247)
	var rawExpiries []sql.NullString
	if err := db.g.WithContext(ctx).Model(&File{}).
		Select("restore_expires_at").
		Where("restore_status = ? AND restore_expires_at IS NOT NULL AND restore_expires_at != ''", RestoreStatusRestored).
		Pluck("restore_expires_at", &rawExpiries).Error; err != nil {
		return s, err
	}
	now := time.Now().UTC()
	var soonestT *time.Time
	for _, raw := range rawExpiries {
		if !raw.Valid || raw.String == "" {
			continue
		}
		t, ok := parseFlexTime(raw.String)
		if !ok {
			slog.Warn("Stats: unparseable restore_expires_at", "value", raw.String)
			continue
		}
		if !t.After(now) {
			continue
		}
		if soonestT == nil || t.Before(*soonestT) {
			tt := t
			soonestT = &tt
		}
	}
	s.RestoreSoonestExp = soonestT

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
			if err := ctx.Err(); err != nil { // cancel-aware (#227)
				return err
			}
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

// MarkPendingByIDsForce resets md5/zip_name/s3_key/uploaded_at and flips
// status to pending for every matching row, regardless of its prior
// status. Used by the authoritative S3 sync when cloud state says the
// object is gone and even a row already marked missing should be
// normalised back to pending so the next source scan can re-evaluate it.
func (db *DB) MarkPendingByIDsForce(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var total int64
	err := db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for len(ids) > 0 {
			if err := ctx.Err(); err != nil { // cancel-aware (#227)
				return err
			}
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
			if err := ctx.Err(); err != nil { // cancel-aware (#227)
				return err
			}
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
	// The engine's PendingFile only reads ID/Path/Size/MTime; pulling the
	// full row (md5/sha256/zip_name/etc.) wastes I/O on large indexes. (#172)
	err := db.g.WithContext(ctx).
		Select("id, path, size, mtime").
		Where("status IN ?", statuses).
		Order("path").
		Find(&files).Error
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

// ListFilesByRestoreStatus returns distinct non-empty s3_keys for files
// whose restore_status matches. Used by the on-demand restore scanner to
// re-check only "in_progress" rows instead of paying for a HEAD on every
// uploaded file.
func (db *DB) ListFilesByRestoreStatus(ctx context.Context, status string) ([]string, error) {
	var keys []string
	err := db.g.WithContext(ctx).Model(&File{}).
		Where("restore_status = ? AND COALESCE(s3_key,'') != ''", status).
		Distinct("s3_key").
		Order("s3_key").
		Pluck("s3_key", &keys).Error
	return keys, err
}

// ListRestoreScanKeys returns distinct non-empty s3_keys for rows that
// represent objects currently present in S3. The authoritative restore
// scan needs both standalone uploads and zip archives, so this includes
// uploaded and zipped rows.
func (db *DB) ListRestoreScanKeys(ctx context.Context) ([]string, error) {
	var keys []string
	err := db.g.WithContext(ctx).Model(&File{}).
		Where("status IN ? AND COALESCE(s3_key,'') != ''", []string{StatusUploaded, StatusZipped}).
		Distinct("s3_key").
		Order("s3_key").
		Pluck("s3_key", &keys).Error
	return keys, err
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
		// Check ctx between chunks so a cancel doesn't keep blocking
		// other writers behind the SQLite write lock for the remaining
		// chunks of a huge reconcile. (#171)
		if err := ctx.Err(); err != nil {
			return total, err
		}
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

// MarkRestoreInProgress flips every row whose s3_key matches to
// restore_status='in_progress'. Rows already marked 'restored' are left
// alone so an out-of-order Post-after-Completed delivery does not regress
// the lifecycle. Returns rows affected.
func (db *DB) MarkRestoreInProgress(ctx context.Context, s3Key string) (int64, error) {
	if s3Key == "" {
		return 0, nil
	}
	// COALESCE so legacy rows with NULL restore_status (DBs pre-goose, or
	// inserts outside GORM) still flip to in_progress — `NULL != 'restored'`
	// evaluates to NULL in SQLite and would silently exclude them. (#265)
	result := db.g.WithContext(ctx).Model(&File{}).
		Where("s3_key = ? AND COALESCE(restore_status,'') != ?", s3Key, RestoreStatusRestored).
		Update("restore_status", RestoreStatusInProgress)
	return result.RowsAffected, result.Error
}

// RestoreEstimateStats aggregates per-bucket counts + bytes for a
// restore-cost estimate in one SQL pass per chunk instead of paging
// the whole index into Go and matching prefixes per row. (#estimate-batch)
// The breakdown splits files by current restore_status so the cost
// estimate (and the trigger) can ignore files already in_progress or
// already restored — re-issuing those would just extend their AWS
// expiry timer and re-bill retrieval. (#estimate-skip-thawed)
//
// `paths` are matched component-wise: a path equals the request OR
// starts with it followed by a separator (so "foo" matches "foo" and
// "foo/bar" but not "foobar"). Forward slash is the canonical
// separator the rest of the system stores.
//
// Pass allFiles=true to skip the path filter entirely (the "/" or ""
// sentinel from the API). MatchedPaths is the subset of `paths` that
// matched at least one uploaded/zipped row (regardless of restore
// state); the caller's "unknown_paths" is the complement.
// RestoreEstimateBreakdown splits the matched files into the bucket the
// caller actually needs to retrieve (Retrievable*) and the bucket already
// thawed or being thawed (AlreadyInProgress* + AlreadyRestored*) so the
// estimate cost / trigger payload only counts files that will actually
// generate fresh S3 RestoreObject calls.
type RestoreEstimateBreakdown struct {
	RetrievableCount       int64
	RetrievableBytes       int64
	AlreadyInProgressCount int64
	AlreadyInProgressBytes int64
	AlreadyRestoredCount   int64
	AlreadyRestoredBytes   int64
	MatchedPaths           []string
}

func (db *DB) RestoreEstimateStats(ctx context.Context, paths []string, allFiles bool) (RestoreEstimateBreakdown, error) {
	statuses := []string{StatusUploaded, StatusZipped}

	// Conditional aggregates so one scan per chunk yields counts + bytes
	// for all three buckets (retrievable / in_progress / restored).
	const sel = "" +
		"SUM(CASE WHEN COALESCE(restore_status,'') NOT IN ('" + RestoreStatusInProgress + "','" + RestoreStatusRestored + "') THEN 1 ELSE 0 END) AS retrievable_count, " +
		"COALESCE(SUM(CASE WHEN COALESCE(restore_status,'') NOT IN ('" + RestoreStatusInProgress + "','" + RestoreStatusRestored + "') THEN size ELSE 0 END), 0) AS retrievable_bytes, " +
		"SUM(CASE WHEN restore_status = '" + RestoreStatusInProgress + "' THEN 1 ELSE 0 END) AS in_progress_count, " +
		"COALESCE(SUM(CASE WHEN restore_status = '" + RestoreStatusInProgress + "' THEN size ELSE 0 END), 0) AS in_progress_bytes, " +
		"SUM(CASE WHEN restore_status = '" + RestoreStatusRestored + "' THEN 1 ELSE 0 END) AS restored_count, " +
		"COALESCE(SUM(CASE WHEN restore_status = '" + RestoreStatusRestored + "' THEN size ELSE 0 END), 0) AS restored_bytes"

	type aggRow struct {
		RetrievableCount int64
		RetrievableBytes int64
		InProgressCount  int64
		InProgressBytes  int64
		RestoredCount    int64
		RestoredBytes    int64
	}
	var br RestoreEstimateBreakdown

	addRow := func(r aggRow) {
		br.RetrievableCount += r.RetrievableCount
		br.RetrievableBytes += r.RetrievableBytes
		br.AlreadyInProgressCount += r.InProgressCount
		br.AlreadyInProgressBytes += r.InProgressBytes
		br.AlreadyRestoredCount += r.RestoredCount
		br.AlreadyRestoredBytes += r.RestoredBytes
	}

	if allFiles || len(paths) == 0 {
		var row aggRow
		if err := db.g.WithContext(ctx).Model(&File{}).
			Where("status IN ?", statuses).
			Select(sel).
			Scan(&row).Error; err != nil {
			return RestoreEstimateBreakdown{}, err
		}
		addRow(row)
		return br, nil
	}

	// Chunk the path filter so the OR-chain stays under SQLite's
	// SQLITE_MAX_EXPR_DEPTH (default 1000). Each path contributes one
	// OR'd subexpression; with thousands of paths a single query blows
	// the limit. (#estimate-depth)
	for s := 0; s < len(paths); s += sqlChunkSize {
		if err := ctx.Err(); err != nil {
			return RestoreEstimateBreakdown{}, err
		}
		end := s + sqlChunkSize
		if end > len(paths) {
			end = len(paths)
		}
		chunk := paths[s:end]
		conds := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*2)
		for _, p := range chunk {
			conds = append(conds, "path = ? OR path LIKE ? ESCAPE '\\'")
			args = append(args, p, likeEscape.Replace(p)+"/%")
		}
		var row aggRow
		if err := db.g.WithContext(ctx).Model(&File{}).
			Where("status IN ?", statuses).
			Where("("+strings.Join(conds, ") OR (")+")", args...).
			Select(sel).
			Scan(&row).Error; err != nil {
			return RestoreEstimateBreakdown{}, err
		}
		addRow(row)
	}

	// Per-path EXISTS check so the caller can flag unmatched paths.
	// "Matched" means "matches any uploaded/zipped row" regardless of
	// restore_status — a path whose files are all already thawed should
	// not be reported as "unknown", just as having nothing to retrieve.
	br.MatchedPaths = make([]string, 0, len(paths))
	for _, p := range paths {
		var probe []int64
		if err := db.g.WithContext(ctx).Model(&File{}).
			Where("status IN ? AND (path = ? OR path LIKE ? ESCAPE '\\')",
				statuses, p, likeEscape.Replace(p)+"/%").
			Select("id").Limit(1).Pluck("id", &probe).Error; err != nil {
			return RestoreEstimateBreakdown{}, err
		}
		if len(probe) > 0 {
			br.MatchedPaths = append(br.MatchedPaths, p)
		}
	}
	return br, nil
}

// MarkRestoreInProgressMany is the bulk variant of MarkRestoreInProgress.
// Runs one UPDATE per chunk of s3_keys so a "restore folder" click that
// covers thousands of rows doesn't serialise into a thousand SQLite
// commits — the original loop took minutes on big zip groups because
// each iteration was its own transaction. (#restore-batch)
func (db *DB) MarkRestoreInProgressMany(ctx context.Context, s3Keys []string) (int64, error) {
	if len(s3Keys) == 0 {
		return 0, nil
	}
	// Dedupe — callers may pass repeats (e.g. every member of a zip
	// group carries the same s3_key) and the WHERE clause matches the
	// same rows on each duplicate, so the work would otherwise scale
	// O(unique_keys × duplicate_factor).
	seen := make(map[string]struct{}, len(s3Keys))
	uniq := make([]string, 0, len(s3Keys))
	for _, k := range s3Keys {
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		uniq = append(uniq, k)
	}
	if len(uniq) == 0 {
		return 0, nil
	}
	var total int64
	err := db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for len(uniq) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			chunk := uniq
			if len(chunk) > sqlChunkSize {
				chunk = uniq[:sqlChunkSize]
			}
			uniq = uniq[len(chunk):]
			res := tx.Model(&File{}).
				Where("s3_key IN ? AND COALESCE(restore_status,'') != ?", chunk, RestoreStatusRestored).
				Update("restore_status", RestoreStatusInProgress)
			if res.Error != nil {
				return res.Error
			}
			total += res.RowsAffected
		}
		return nil
	})
	return total, err
}

// MarkRestoreCleared resets restore_status / restore_expires_at on every
// row whose s3_key matches and is currently 'in_progress' or 'restored'.
// Used when the restore scanner observes a HEAD with no x-amz-restore
// header — i.e. the temporary restored copy has expired or the object
// was never actually being restored — so the UI doesn't keep showing
// "thawing forever" for an in_progress row whose Completed event was
// missed and whose expiry has since elapsed. (#246)
func (db *DB) MarkRestoreCleared(ctx context.Context, s3Key string) (int64, error) {
	if s3Key == "" {
		return 0, nil
	}
	result := db.g.WithContext(ctx).Model(&File{}).
		Where("s3_key = ? AND restore_status IN ?", s3Key, []string{RestoreStatusInProgress, RestoreStatusRestored}).
		Updates(map[string]any{
			"restore_status":     "",
			"restore_expires_at": nil,
		})
	return result.RowsAffected, result.Error
}

// MarkRestored flips every row whose s3_key matches to
// restore_status='restored' and records the temporary copy's expiry time.
func (db *DB) MarkRestored(ctx context.Context, s3Key string, expiresAt time.Time) (int64, error) {
	if s3Key == "" {
		return 0, nil
	}
	result := db.g.WithContext(ctx).Model(&File{}).
		Where("s3_key = ?", s3Key).
		Updates(map[string]any{
			"restore_status":     RestoreStatusRestored,
			"restore_expires_at": expiresAt,
		})
	return result.RowsAffected, result.Error
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
			if err := ctx.Err(); err != nil { // cancel-aware (#227)
				return err
			}
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
