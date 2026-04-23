package db

import (
	"context"
	"database/sql"
	"time"
)

// Run lifecycle statuses.
const (
	RunRunning   = "running"
	RunCompleted = "completed"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

// Log severity levels.
const (
	LogInfo  = "info"
	LogWarn  = "warn"
	LogError = "error"
)

// Run is the typed row for the `runs` table.
type Run struct {
	ID             int64
	StartedAt      time.Time
	FinishedAt     time.Time // zero = still running
	Status         string
	FilesScanned   int64
	FilesUploaded  int64
	BytesUploaded  int64
	ErrorMessage   string
}

// RunLog is the typed row for `run_logs`.
type RunLog struct {
	ID        int64
	RunID     int64
	Timestamp time.Time
	Level     string
	Message   string
}

// CreateRun inserts a new run row in `running` state.
func (db *DB) CreateRun(ctx context.Context, startedAt time.Time) (int64, error) {
	r, err := db.ExecContext(ctx,
		`INSERT INTO runs (started_at, status) VALUES (?, ?)`,
		isoTime(startedAt), RunRunning,
	)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// UpdateRunStats bumps counters without touching status or finished_at.
func (db *DB) UpdateRunStats(ctx context.Context, runID, scanned, uploaded, bytes int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE runs
		   SET files_scanned = ?, files_uploaded = ?, bytes_uploaded = ?
		 WHERE id = ?`,
		scanned, uploaded, bytes, runID,
	)
	return err
}

// FinishRun stamps finished_at and sets terminal status + optional error.
func (db *DB) FinishRun(ctx context.Context, runID int64, status, errorMsg string, finishedAt time.Time) error {
	var errPtr any
	if errorMsg != "" {
		errPtr = errorMsg
	}
	_, err := db.ExecContext(ctx,
		`UPDATE runs
		   SET finished_at = ?, status = ?, error_message = ?
		 WHERE id = ?`,
		isoTime(finishedAt), status, errPtr, runID,
	)
	return err
}

// GetRun returns a single run. Returns sql.ErrNoRows if id is unknown.
func (db *DB) GetRun(ctx context.Context, id int64) (Run, error) {
	var (
		r          Run
		finishedAt sql.NullString
		errMsg     sql.NullString
		startedAt  string
	)
	err := db.QueryRowContext(ctx,
		`SELECT id, started_at, finished_at, status,
		        files_scanned, files_uploaded, bytes_uploaded, error_message
		   FROM runs WHERE id = ?`, id,
	).Scan(&r.ID, &startedAt, &finishedAt, &r.Status,
		&r.FilesScanned, &r.FilesUploaded, &r.BytesUploaded, &errMsg)
	if err != nil {
		return r, err
	}
	r.StartedAt, _ = parseTime(startedAt)
	if finishedAt.Valid {
		r.FinishedAt, _ = parseTime(finishedAt.String)
	}
	if errMsg.Valid {
		r.ErrorMessage = errMsg.String
	}
	return r, nil
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
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, started_at, COALESCE(finished_at,''), status,
		        files_scanned, files_uploaded, bytes_uploaded,
		        COALESCE(error_message,'')
		   FROM runs
		 ORDER BY id DESC
		 LIMIT ? OFFSET ?`,
		limit, (page-1)*limit,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var (
			r          Run
			startedAt  string
			finishedAt string
		)
		if err := rows.Scan(&r.ID, &startedAt, &finishedAt, &r.Status,
			&r.FilesScanned, &r.FilesUploaded, &r.BytesUploaded, &r.ErrorMessage); err != nil {
			return nil, 0, err
		}
		r.StartedAt, _ = parseTime(startedAt)
		r.FinishedAt, _ = parseTime(finishedAt)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// AppendLog writes a log line bound to a run.
func (db *DB) AppendLog(ctx context.Context, runID int64, level, message string, at time.Time) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO run_logs (run_id, timestamp, level, message)
		 VALUES (?, ?, ?, ?)`,
		runID, isoTime(at), level, message,
	)
	return err
}

// LogEntry is one buffered log line for AppendLogMany.
type LogEntry struct {
	RunID   int64
	At      time.Time
	Level   string
	Message string
}

// AppendLogMany inserts many log rows in a single transaction. The engine
// buffers log entries in memory and calls this periodically so per-file
// log spam doesn't generate one WAL fsync per line.
func (db *DB) AppendLogMany(ctx context.Context, entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO run_logs (run_id, timestamp, level, message) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, e := range entries {
		if _, err := stmt.ExecContext(ctx, e.RunID, isoTime(e.At), e.Level, e.Message); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListLogs returns all log lines for a run in chronological order.
func (db *DB) ListLogs(ctx context.Context, runID int64) ([]RunLog, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, run_id, timestamp, level, message
		   FROM run_logs
		  WHERE run_id = ?
		 ORDER BY id`, runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunLog
	for rows.Next() {
		var (
			l   RunLog
			tsS string
		)
		if err := rows.Scan(&l.ID, &l.RunID, &tsS, &l.Level, &l.Message); err != nil {
			return nil, err
		}
		l.Timestamp, _ = parseTime(tsS)
		out = append(out, l)
	}
	return out, rows.Err()
}
