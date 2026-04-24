package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// File statuses — authoritative list.
const (
	StatusPending  = "pending"
	StatusZipped   = "zipped"
	StatusUploaded = "uploaded"
	StatusFailed   = "failed"
	StatusMissing  = "missing"
)

// File is the typed row for the `files` table.
type File struct {
	ID         int64
	Path       string
	Size       int64
	MTime      time.Time
	MD5        string
	Status     string
	ZipName    string
	S3Key      string
	UploadedAt time.Time
	LastSeenAt time.Time
}

// UpsertResult captures what changed during UpsertFile — the engine uses it
// to decide whether to recompute md5 / re-queue the file for upload.
type UpsertResult struct {
	ID      int64
	Created bool // row did not exist before
	Changed bool // size or mtime differ from stored row
}

// BatchEntry is a single file record passed to UpsertFileBatch.
type BatchEntry struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// UpsertFileBatch processes many entries in a single transaction for performance.
// Returns one UpsertResult per entry in the same order.
func (db *DB) UpsertFileBatch(ctx context.Context, entries []BatchEntry, seenAt time.Time) ([]UpsertResult, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	results := make([]UpsertResult, len(entries))

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for i, e := range entries {
		var (
			existingID   int64
			existingSize int64
			existingMT   string
		)
		err = tx.QueryRowContext(ctx,
			`SELECT id, size, mtime FROM files WHERE path = ?`, e.Path,
		).Scan(&existingID, &existingSize, &existingMT)

		switch {
		case err == sql.ErrNoRows:
			r, err := tx.ExecContext(ctx,
				`INSERT INTO files (path, size, mtime, status, last_seen_at)
				 VALUES (?, ?, ?, ?, ?)`,
				e.Path, e.Size, isoTime(e.ModTime), StatusPending, isoTime(seenAt),
			)
			if err != nil {
				return nil, err
			}
			results[i].ID, _ = r.LastInsertId()
			results[i].Created = true
			results[i].Changed = true

		case err != nil:
			return nil, err

		default:
			results[i].ID = existingID
			storedMT, _ := parseTime(existingMT)
			if e.Size != existingSize || !storedMT.Equal(e.ModTime) {
				results[i].Changed = true
				_, err = tx.ExecContext(ctx,
					`UPDATE files
					   SET size = ?, mtime = ?, status = ?, md5 = NULL,
					       zip_name = NULL, s3_key = NULL, uploaded_at = NULL,
					       last_seen_at = ?
					 WHERE id = ?`,
					e.Size, isoTime(e.ModTime), StatusPending, isoTime(seenAt), existingID,
				)
			} else {
				_, err = tx.ExecContext(ctx,
					`UPDATE files SET last_seen_at = ? WHERE id = ?`,
					isoTime(seenAt), existingID,
				)
			}
			if err != nil {
				return nil, err
			}
		}
	}

	return results, tx.Commit()
}

// UpsertFile inserts or updates the path row. It always refreshes
// last_seen_at. If size or mtime changed (or the row is new) the status is
// reset to pending and md5/zip_name/s3_key/uploaded_at are cleared so the
// engine will re-upload.
func (db *DB) UpsertFile(ctx context.Context, path string, size int64, mtime, seenAt time.Time) (UpsertResult, error) {
	var res UpsertResult

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	var (
		existingID   int64
		existingSize int64
		existingMT   string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, size, mtime FROM files WHERE path = ?`, path,
	).Scan(&existingID, &existingSize, &existingMT)

	switch {
	case err == sql.ErrNoRows:
		r, err := tx.ExecContext(ctx,
			`INSERT INTO files (path, size, mtime, status, last_seen_at)
			 VALUES (?, ?, ?, ?, ?)`,
			path, size, isoTime(mtime), StatusPending, isoTime(seenAt),
		)
		if err != nil {
			return res, err
		}
		res.ID, _ = r.LastInsertId()
		res.Created = true
		res.Changed = true

	case err != nil:
		return res, err

	default:
		res.ID = existingID
		storedMT, _ := parseTime(existingMT)
		if size != existingSize || !storedMT.Equal(mtime) {
			res.Changed = true
			_, err = tx.ExecContext(ctx,
				`UPDATE files
				   SET size = ?, mtime = ?, status = ?, md5 = NULL,
				       zip_name = NULL, s3_key = NULL, uploaded_at = NULL,
				       last_seen_at = ?
				 WHERE id = ?`,
				size, isoTime(mtime), StatusPending, isoTime(seenAt), existingID,
			)
		} else {
			_, err = tx.ExecContext(ctx,
				`UPDATE files SET last_seen_at = ? WHERE id = ?`,
				isoTime(seenAt), existingID,
			)
		}
		if err != nil {
			return res, err
		}
	}

	return res, tx.Commit()
}

// MarkMissing flips any previously uploaded row whose last_seen_at is older
// than scanStart to status=missing. Returns the affected row count.
func (db *DB) MarkMissing(ctx context.Context, scanStart time.Time) (int64, error) {
	r, err := db.ExecContext(ctx,
		`UPDATE files
		   SET status = ?
		 WHERE status = ? AND last_seen_at < ?`,
		StatusMissing, StatusUploaded, isoTime(scanStart),
	)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// MarkUploaded sets md5, s3_key, uploaded_at and flips status to uploaded.
func (db *DB) MarkUploaded(ctx context.Context, id int64, md5, s3Key string, uploadedAt time.Time) error {
	_, err := db.ExecContext(ctx,
		`UPDATE files
		   SET md5 = ?, s3_key = ?, uploaded_at = ?, status = ?
		 WHERE id = ?`,
		md5, s3Key, isoTime(uploadedAt), StatusUploaded, id,
	)
	return err
}

// MarkUploadedBatch flips many ids to 'uploaded' sharing a single md5 /
// s3_key / uploaded_at — used for zip groups where every file lands in
// the same S3 object. One UPDATE replaces N separate transactions.
func (db *DB) MarkUploadedBatch(ctx context.Context, ids []int64, md5, s3Key string, uploadedAt time.Time) error {
	for len(ids) > 0 {
		chunk := ids
		if len(chunk) > sqlChunkSize {
			chunk = ids[:sqlChunkSize]
		}
		ids = ids[len(chunk):]

		args := make([]any, 0, len(chunk)+4)
		args = append(args, md5, s3Key, isoTime(uploadedAt), StatusUploaded)
		for _, id := range chunk {
			args = append(args, id)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE files SET md5 = ?, s3_key = ?, uploaded_at = ?, status = ?
			 WHERE id IN `+inPlaceholders(len(chunk)),
			args...,
		); err != nil {
			return err
		}
	}
	return nil
}

// UploadedRow describes one individual-upload outcome to flush in a batch.
type UploadedRow struct {
	ID         int64
	MD5        string
	S3Key      string
	UploadedAt time.Time
}

// MarkUploadedMany applies per-file uploaded state in a single transaction.
// Used by the engine to drain a buffer of individual uploads, replacing N
// separate WAL fsyncs with one commit.
func (db *DB) MarkUploadedMany(ctx context.Context, rows []UploadedRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx,
			`UPDATE files
			   SET md5 = ?, s3_key = ?, uploaded_at = ?, status = ?
			 WHERE id = ?`,
			r.MD5, r.S3Key, isoTime(r.UploadedAt), StatusUploaded, r.ID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkFailed sets the file status to failed (engine records the reason in run_logs).
func (db *DB) MarkFailed(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE files SET status = ? WHERE id = ?`, StatusFailed, id,
	)
	return err
}

// sqlChunkSize is the maximum number of values we put in a single
// IN (?, …) clause. SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 999;
// we stay well under it to leave room for the other bound parameters.
const sqlChunkSize = 500

// inPlaceholders builds a "(?,?,…)" string of n question marks.
func inPlaceholders(n int) string {
	s := strings.Repeat("?,", n)
	return "(" + s[:len(s)-1] + ")"
}

// SetZipName attaches a zip manifest name to a batch of file IDs and
// flips their status to zipped. IDs are processed in chunks to stay
// under SQLite's bound-variable limit.
func (db *DB) SetZipName(ctx context.Context, ids []int64, zipName string) error {
	for len(ids) > 0 {
		chunk := ids
		if len(chunk) > sqlChunkSize {
			chunk = ids[:sqlChunkSize]
		}
		ids = ids[len(chunk):]

		args := make([]any, 0, len(chunk)+2)
		args = append(args, zipName, StatusZipped)
		for _, id := range chunk {
			args = append(args, id)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE files SET zip_name = ?, status = ? WHERE id IN `+inPlaceholders(len(chunk)),
			args...,
		); err != nil {
			return err
		}
	}
	return nil
}

// FilesFilter describes a ListFiles query.
type FilesFilter struct {
	Status string
	Search string // substring match on path
	Page   int    // 1-based
	Limit  int
	// All disables pagination so the tree view can build a full hierarchy
	// without the caller having to fetch every page manually.
	All bool
}

// ListFiles returns a page of file rows matching filter, plus the total row count.
func (db *DB) ListFiles(ctx context.Context, f FilesFilter) ([]File, int64, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Page <= 0 {
		f.Page = 1
	}

	var (
		where []string
		args  []any
	)
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.Search != "" {
		where = append(where, "path LIKE ?")
		args = append(args, "%"+f.Search+"%")
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}

	var total int64
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM files "+whereSQL, args...,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := fmt.Sprintf(
		`SELECT id, path, size, mtime, COALESCE(md5,''), status,
		        COALESCE(zip_name,''), COALESCE(s3_key,''),
		        COALESCE(uploaded_at,''), last_seen_at
		   FROM files %s
		 ORDER BY path`, whereSQL,
	)
	qargs := args
	if !f.All {
		query += ` LIMIT ? OFFSET ?`
		qargs = append(qargs, f.Limit, (f.Page-1)*f.Limit)
	}
	rows, err := db.QueryContext(ctx, query, qargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []File
	for rows.Next() {
		var (
			f          File
			mtimeStr   string
			uploadedAt string
			lastSeen   string
		)
		if err := rows.Scan(&f.ID, &f.Path, &f.Size, &mtimeStr, &f.MD5, &f.Status,
			&f.ZipName, &f.S3Key, &uploadedAt, &lastSeen); err != nil {
			return nil, 0, err
		}
		f.MTime, _ = parseTime(mtimeStr)
		f.UploadedAt, _ = parseTime(uploadedAt)
		f.LastSeenAt, _ = parseTime(lastSeen)
		out = append(out, f)
	}
	return out, total, rows.Err()
}

// FileStats is the aggregate view for the dashboard.
type FileStats struct {
	ByStatus   map[string]int64
	TotalSize  int64
	TotalCount int64
}

// MarkPendingByIDs resets md5/zip_name/s3_key/uploaded_at and flips
// status to 'pending' for the given ids — used when the user clicks
// retry on a failed (or any) row so the next run picks it up.
func (db *DB) MarkPendingByIDs(ctx context.Context, ids []int64) (int64, error) {
	var total int64
	for len(ids) > 0 {
		chunk := ids
		if len(chunk) > sqlChunkSize {
			chunk = ids[:sqlChunkSize]
		}
		ids = ids[len(chunk):]

		args := make([]any, 0, len(chunk)+1)
		args = append(args, StatusPending)
		for _, id := range chunk {
			args = append(args, id)
		}
		r, err := db.ExecContext(ctx,
			`UPDATE files
			   SET status = ?, md5 = NULL, zip_name = NULL, s3_key = NULL, uploaded_at = NULL
			 WHERE id IN `+inPlaceholders(len(chunk)),
			args...,
		)
		if err != nil {
			return total, err
		}
		n, _ := r.RowsAffected()
		total += n
	}
	return total, nil
}

// PurgeMissingFiles deletes every row whose status is 'missing'. These
// are files that were on disk when first scanned but later removed; once
// they are also gone from S3 there is nothing left to track.
func (db *DB) PurgeMissingFiles(ctx context.Context) (int64, error) {
	r, err := db.ExecContext(ctx, `DELETE FROM files WHERE status = ?`, StatusMissing)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// MarkAllFailedPending is the bulk 'retry everything that failed' path.
func (db *DB) MarkAllFailedPending(ctx context.Context) (int64, error) {
	r, err := db.ExecContext(ctx,
		`UPDATE files
		   SET status = ?, md5 = NULL, zip_name = NULL, s3_key = NULL, uploaded_at = NULL
		 WHERE status = ?`,
		StatusPending, StatusFailed,
	)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// DeleteFiles removes the given rows from the files table. Does not
// touch S3 — the historical object stays where it is (Deep Archive
// billing model means we never delete anyway).
func (db *DB) DeleteFiles(ctx context.Context, ids []int64) (int64, error) {
	var total int64
	for len(ids) > 0 {
		chunk := ids
		if len(chunk) > sqlChunkSize {
			chunk = ids[:sqlChunkSize]
		}
		ids = ids[len(chunk):]

		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		r, err := db.ExecContext(ctx, `DELETE FROM files WHERE id IN `+inPlaceholders(len(chunk)), args...)
		if err != nil {
			return total, err
		}
		n, _ := r.RowsAffected()
		total += n
	}
	return total, nil
}

// ListPending returns every row the engine should attempt to upload on
// the next run. Always includes 'pending'; when includeFailed is true
// it also includes 'failed' rows so they get retried automatically.
func (db *DB) ListPending(ctx context.Context, includeFailed bool) ([]File, error) {
	query := `SELECT id, path, size, mtime, COALESCE(md5,''), status,
		             COALESCE(zip_name,''), COALESCE(s3_key,''),
		             COALESCE(uploaded_at,''), last_seen_at
		        FROM files
		       WHERE status = ?`
	args := []any{StatusPending}
	if includeFailed {
		query += ` OR status = ?`
		args = append(args, StatusFailed)
	}
	query += ` ORDER BY path`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var (
			f          File
			mtimeStr   string
			uploadedAt string
			lastSeen   string
		)
		if err := rows.Scan(&f.ID, &f.Path, &f.Size, &mtimeStr, &f.MD5, &f.Status,
			&f.ZipName, &f.S3Key, &uploadedAt, &lastSeen); err != nil {
			return nil, err
		}
		f.MTime, _ = parseTime(mtimeStr)
		f.UploadedAt, _ = parseTime(uploadedAt)
		f.LastSeenAt, _ = parseTime(lastSeen)
		out = append(out, f)
	}
	return out, rows.Err()
}

// Stats returns per-status counts plus total size/count across the index.
func (db *DB) Stats(ctx context.Context) (FileStats, error) {
	s := FileStats{ByStatus: map[string]int64{}}

	rows, err := db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM files GROUP BY status`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return s, err
		}
		s.ByStatus[status] = count
		s.TotalCount += count
	}
	if err := rows.Err(); err != nil {
		return s, err
	}

	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size),0) FROM files`,
	).Scan(&s.TotalSize); err != nil {
		return s, err
	}
	return s, nil
}

// ListZipNames returns every distinct non-empty zip_name in the index.
// Each name represents one zip object in S3 that may cover many files.
func (db *DB) ListZipNames(ctx context.Context) ([]string, error) {
	return listDistinctStrings(ctx, db.DB,
		`SELECT DISTINCT zip_name FROM files WHERE zip_name != '' ORDER BY zip_name`)
}

// ListIndividualS3Keys returns distinct s3_key values for files that were
// uploaded individually (zip_name is empty or NULL). Zipped files are
// excluded because their S3 object is identified by zip_name, not s3_key.
// COALESCE is required because UpsertFileBatch inserts with zip_name left
// at its NULL default, and SQLite's `= ”` does not match NULL.
func (db *DB) ListIndividualS3Keys(ctx context.Context) ([]string, error) {
	return listDistinctStrings(ctx, db.DB,
		`SELECT DISTINCT s3_key FROM files
		  WHERE COALESCE(zip_name,'') = '' AND COALESCE(s3_key,'') != ''
		  ORDER BY s3_key`)
}

func listDistinctStrings(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MarkPendingByPaths resets files to pending by exact path match,
// regardless of their current status. Used to force re-upload of files
// that are present locally but absent from the cloud index.
func (db *DB) MarkPendingByPaths(ctx context.Context, paths []string) (int64, error) {
	return markPendingByStrings(ctx, db, "path", paths)
}

// MarkPendingByZipNames resets all files whose zip_name is in the list.
// Used when a zip object is confirmed missing from S3.
func (db *DB) MarkPendingByZipNames(ctx context.Context, zipNames []string) (int64, error) {
	return markPendingByStrings(ctx, db, "zip_name", zipNames)
}

// MarkPendingByS3Keys resets individually-uploaded files whose s3_key is in
// the list. Used when a directly-uploaded object is missing from S3.
func (db *DB) MarkPendingByS3Keys(ctx context.Context, keys []string) (int64, error) {
	return markPendingByStrings(ctx, db, "s3_key", keys)
}

// markPendingByStrings is the shared implementation for the three
// MarkPendingBy* functions. It processes values in chunks so we never
// exceed SQLite's bound-variable limit.
// Files with status='missing' are excluded: they no longer exist on
// disk and cannot be re-uploaded.
func markPendingByStrings(ctx context.Context, db *DB, column string, values []string) (int64, error) {
	var total int64
	for len(values) > 0 {
		chunk := values
		if len(chunk) > sqlChunkSize {
			chunk = values[:sqlChunkSize]
		}
		values = values[len(chunk):]

		args := make([]any, 0, len(chunk)+2)
		args = append(args, StatusPending, StatusMissing)
		for _, v := range chunk {
			args = append(args, v)
		}
		res, err := db.ExecContext(ctx,
			`UPDATE files
			   SET status = ?, md5 = NULL, zip_name = NULL, s3_key = NULL, uploaded_at = NULL
			 WHERE status != ? AND `+column+` IN `+inPlaceholders(len(chunk)),
			args...,
		)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
