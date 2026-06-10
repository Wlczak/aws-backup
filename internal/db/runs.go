package db

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Run lifecycle statuses.
const (
	RunRunning   = "running"
	RunCompleted = "completed"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
	// RunStopped is the terminal state for a run that exited cleanly
	// after a graceful stop request: the in-flight upload finished, but
	// no further files were started. Distinct from RunCancelled (force
	// kill mid-upload). (#124)
	RunStopped = "stopped"
)

// Log severity levels.
const (
	LogInfo  = "info"
	LogWarn  = "warn"
	LogError = "error"
)

// Run is the GORM model for the `runs` table.
type Run struct {
	ID            int64     `gorm:"column:id;primaryKey;autoIncrement"`
	StartedAt     time.Time `gorm:"column:started_at;not null;index"`
	FinishedAt    time.Time `gorm:"column:finished_at"`
	Status        string    `gorm:"column:status;not null;default:'running';index"`
	FilesScanned  int64     `gorm:"column:files_scanned;not null;default:0"`
	BytesScanned  int64     `gorm:"column:bytes_scanned;not null;default:0"`
	FilesUploaded int64     `gorm:"column:files_uploaded;not null;default:0"`
	BytesUploaded int64     `gorm:"column:bytes_uploaded;not null;default:0"`
	FilesPlanned  int64     `gorm:"column:files_planned;not null;default:0"`
	BytesPlanned  int64     `gorm:"column:bytes_planned;not null;default:0"`
	ScanPaused    bool      `gorm:"column:scan_paused;not null;default:false"`
	ScanComplete  bool      `gorm:"column:scan_complete;not null;default:false"`
	ErrorMessage  string    `gorm:"column:error_message"`
}

// RunLog is the GORM model for `run_logs`.
type RunLog struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	RunID     int64     `gorm:"column:run_id;not null;index"`
	Timestamp time.Time `gorm:"column:timestamp;not null"`
	Level     string    `gorm:"column:level;not null"`
	Message   string    `gorm:"column:message;not null"`
}

// RunScanFolder tracks one directory subtree that was fully scanned during a
// run. The table doubles as a persistent per-profile cache for later full
// runs: completed rows are retained after finalize and seed the next batch
// walk unless the caller explicitly bypasses the cache.
type RunScanFolder struct {
	RunID         int64      `gorm:"column:run_id;primaryKey"`
	Path          string     `gorm:"column:path;primaryKey"`
	LastScannedAt time.Time  `gorm:"column:last_scanned_at;not null"`
	CompletedAt   *time.Time `gorm:"column:completed_at"`
}

// CreateRun inserts a new run row in 'running' state. FinishedAt is omitted
// so the column stays NULL until FinishRun is called.
func (db *DB) CreateRun(ctx context.Context, startedAt time.Time) (int64, error) {
	run := Run{StartedAt: startedAt, Status: RunRunning}
	err := db.g.WithContext(ctx).Omit("FinishedAt").Create(&run).Error
	return run.ID, err
}

// UpdateRunStats bumps counters without touching status or finished_at.
func (db *DB) UpdateRunStats(ctx context.Context, runID, scanned, scannedBytes, uploaded, uploadedBytes int64) error {
	return db.g.WithContext(ctx).Model(&Run{}).Where("id = ?", runID).Updates(map[string]any{
		"files_scanned":  scanned,
		"bytes_scanned":  scannedBytes,
		"files_uploaded": uploaded,
		"bytes_uploaded": uploadedBytes,
	}).Error
}

// UpdateRunScanState updates the scan-paused / scan-complete markers for a
// run so /api/status can reflect whether the current run is between scan
// batches or done scanning entirely.
func (db *DB) UpdateRunScanState(ctx context.Context, runID int64, paused, complete bool) error {
	return db.g.WithContext(ctx).Model(&Run{}).Where("id = ?", runID).Updates(map[string]any{
		"scan_paused":   paused,
		"scan_complete": complete,
	}).Error
}

// UpdateUploadStats updates only the upload counters, leaving files_scanned
// untouched. Used during the upload loop so per-group progress writes don't
// clobber the scan count captured at the end of phase 1.
func (db *DB) UpdateUploadStats(ctx context.Context, runID, uploaded, bytes int64) error {
	return db.g.WithContext(ctx).Model(&Run{}).Where("id = ?", runID).Updates(map[string]any{
		"files_uploaded": uploaded,
		"bytes_uploaded": bytes,
	}).Error
}

// SetRunPlan records the planned upload total (file count and byte
// total) for a run. Written once at upload-plan time so a mid-run
// reload can recover the progress denominator. (#dashboard-replay)
func (db *DB) SetRunPlan(ctx context.Context, runID, files, bytes int64) error {
	return db.g.WithContext(ctx).Model(&Run{}).Where("id = ?", runID).Updates(map[string]any{
		"files_planned": files,
		"bytes_planned": bytes,
	}).Error
}

// MarkRunScanFoldersComplete upserts the directory subtrees that finished in
// the current scan batch, along with the time they were completed.
func (db *DB) MarkRunScanFoldersComplete(ctx context.Context, runID int64, paths []string, at time.Time) error {
	if len(paths) == 0 {
		return nil
	}
	rows := make([]RunScanFolder, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		completed := at
		rows = append(rows, RunScanFolder{
			RunID:         runID,
			Path:          p,
			LastScannedAt: at,
			CompletedAt:   &completed,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	return db.g.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "run_id"}, {Name: "path"}},
				DoUpdates: clause.AssignmentColumns([]string{"last_scanned_at", "completed_at"}),
			}).Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// ListRunScanFolderPaths returns every completed subtree path recorded for a
// run. The engine uses this as the skip-set for the next scan batch.
func (db *DB) ListRunScanFolderPaths(ctx context.Context, runID int64) ([]string, error) {
	var rows []RunScanFolder
	if err := db.g.WithContext(ctx).
		Where("run_id = ? AND completed_at IS NOT NULL", runID).
		Order("path ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Path)
	}
	return out, nil
}

// ListCompletedScanFolderPaths returns every completed subtree path recorded
// in the current profile DB, deduplicated across runs. Batched full runs seed
// their initial skip-set from this durable cache.
func (db *DB) ListCompletedScanFolderPaths(ctx context.Context) ([]string, error) {
	var rows []RunScanFolder
	if err := db.g.WithContext(ctx).
		Where("completed_at IS NOT NULL").
		Order("path ASC, completed_at DESC, run_id DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(rows))
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.Path]; ok {
			continue
		}
		seen[row.Path] = struct{}{}
		out = append(out, row.Path)
	}
	return out, nil
}

// ClearCompletedScanFolderPaths removes the persistent completed-folder cache
// across the active profile DB. The engine repopulates it on future full runs.
func (db *DB) ClearCompletedScanFolderPaths(ctx context.Context) (int64, error) {
	res := db.g.WithContext(ctx).
		Where("completed_at IS NOT NULL").
		Delete(&RunScanFolder{})
	return res.RowsAffected, res.Error
}

// FinishRun stamps finished_at and sets terminal status + optional error.
func (db *DB) FinishRun(ctx context.Context, runID int64, status, errorMsg string, finishedAt time.Time) error {
	return db.g.WithContext(ctx).Model(&Run{}).Where("id = ?", runID).Updates(map[string]any{
		"finished_at":   finishedAt,
		"status":        status,
		"error_message": errorMsg,
	}).Error
}

// GetRun returns a single run. Returns gorm.ErrRecordNotFound if id is unknown.
func (db *DB) GetRun(ctx context.Context, id int64) (Run, error) {
	var run Run
	err := db.g.WithContext(ctx).First(&run, id).Error
	return run, err
}

// ListRuns returns newest-first paginated runs plus total count.
func (db *DB) ListRuns(ctx context.Context, page, limit int) ([]Run, int64, error) {
	if limit <= 0 {
		limit = 20
	}
	if page <= 0 {
		page = 1
	}
	var total int64
	if err := db.g.WithContext(ctx).Model(&Run{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var runs []Run
	err := db.g.WithContext(ctx).
		Order("id DESC").
		Offset((page - 1) * limit).
		Limit(limit).
		Find(&runs).Error
	return runs, total, err
}

// AppendLog writes a log line bound to a run.
func (db *DB) AppendLog(ctx context.Context, runID int64, level, message string, at time.Time) error {
	return db.g.WithContext(ctx).Create(&RunLog{
		RunID: runID, Timestamp: at, Level: level, Message: message,
	}).Error
}

// LogEntry is one buffered log line for AppendLogMany.
type LogEntry struct {
	RunID   int64
	At      time.Time
	Level   string
	Message string
}

// AppendLogMany inserts many log rows in a single transaction.
func (db *DB) AppendLogMany(ctx context.Context, entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	logs := make([]RunLog, len(entries))
	for i, e := range entries {
		logs[i] = RunLog{RunID: e.RunID, Timestamp: e.At, Level: e.Level, Message: e.Message}
	}
	return db.g.WithContext(ctx).CreateInBatches(logs, 200).Error
}

// ListLogsMaxLimit caps a single ListLogs page so a long, chatty run's
// log table can't OOM the process when the run-detail UI opens. (#223)
const ListLogsMaxLimit = 5000

// TrimRunLogsForRun caps run_logs.run_id == runID to maxLines rows. When
// the run has more, deletes the lowest-severity oldest rows first
// (info before warn before error) so a chatty info stream can't bury
// the warns/errors useful for post-mortem. No-op for maxLines <= 0.
// Returns the number of rows deleted.
func (db *DB) TrimRunLogsForRun(ctx context.Context, runID int64, maxLines int) (int64, error) {
	if maxLines <= 0 {
		return 0, nil
	}
	var total int64
	if err := db.g.WithContext(ctx).Model(&RunLog{}).
		Where("run_id = ?", runID).
		Count(&total).Error; err != nil {
		return 0, err
	}
	excess := total - int64(maxLines)
	if excess <= 0 {
		return 0, nil
	}
	// Order by severity ASC (info first) then id ASC (oldest first within
	// a tier). LIMIT picks exactly the excess; DELETE removes them.
	res := db.g.WithContext(ctx).Exec(`
		DELETE FROM run_logs
		WHERE id IN (
			SELECT id FROM run_logs
			WHERE run_id = ?
			ORDER BY
				CASE level WHEN 'error' THEN 2 WHEN 'warn' THEN 1 ELSE 0 END ASC,
				id ASC
			LIMIT ?
		)
	`, runID, excess)
	return res.RowsAffected, res.Error
}

// TrimRunLogsByAge deletes every run_logs row whose owning run finished
// before cutoff. The runs row itself is preserved so the dashboard's
// run history (and the row's terminal error_message) is not lost.
// Active (still-running) runs are skipped — their finished_at is the
// zero time. Returns the number of rows deleted.
func (db *DB) TrimRunLogsByAge(ctx context.Context, cutoff time.Time) (int64, error) {
	if cutoff.IsZero() {
		return 0, nil
	}
	res := db.g.WithContext(ctx).Exec(`
		DELETE FROM run_logs
		WHERE run_id IN (
			SELECT id FROM runs
			WHERE finished_at IS NOT NULL
			  AND finished_at != ''
			  AND finished_at < ?
		)
	`, cutoff)
	return res.RowsAffected, res.Error
}

// DeleteRunLogs removes every row from run_logs while leaving runs intact.
// Used by the Logs page's "clear all" action. Returns the number of rows
// deleted.
func (db *DB) DeleteRunLogs(ctx context.Context) (int64, error) {
	res := db.g.WithContext(ctx).Exec(`DELETE FROM run_logs`)
	return res.RowsAffected, res.Error
}

// ListLogs returns up to limit log lines for a run, ordered by id, with
// a 1-based page offset. limit <= 0 selects a 500-row default and is
// capped at ListLogsMaxLimit. Returns the rows + the total count for
// pagination. (#223)
func (db *DB) ListLogs(ctx context.Context, runID int64, page, limit int) ([]RunLog, int64, error) {
	if limit <= 0 {
		limit = 500
	}
	if limit > ListLogsMaxLimit {
		limit = ListLogsMaxLimit
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * limit

	var total int64
	if err := db.g.WithContext(ctx).Model(&RunLog{}).
		Where("run_id = ?", runID).
		Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var logs []RunLog
	err := db.g.WithContext(ctx).
		Where("run_id = ?", runID).
		Order("id").
		Limit(limit).Offset(offset).
		Find(&logs).Error
	return logs, total, err
}
