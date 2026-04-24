package engine

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/source"
	"github.com/Wlczak/aws-backup/internal/storage"
)

// ZipIndexSuffix is the suffix appended to a zip's S3 key to form the
// sidecar that lists the archive's contents. STANDARD-tier so listing
// doesn't require a Glacier restore.
const ZipIndexSuffix = ".index.txt"

const zipIndexSuffix = ZipIndexSuffix

// RunMode controls which phases of a backup cycle execute.
type RunMode string

const (
	// RunModeFull runs scan then upload (default).
	RunModeFull RunMode = "full"
	// RunModeScan runs only the scan phase; pending files are written to DB
	// but not uploaded. When ScanPaths is set, only matching files are
	// re-scanned (partial rescan) and missing-detection is skipped.
	RunModeScan RunMode = "scan"
	// RunModeUpload skips the scan and uploads all currently pending files.
	RunModeUpload RunMode = "upload"
)

// Options wires the engine to the outside world.
type Options struct {
	DB        *db.DB
	Source    source.Source
	Storage   storage.Storage
	TmpDir    string
	KeyPrefix string // e.g. "backups/"
	ChunkSize int    // how many individual files to upload per batch
	ZipThresh int    // files in a top-dir group >= this -> zip
	// ZipMaxBytes caps the uncompressed byte total of a single zip
	// group. Subtrees larger than this are split along subdirectory
	// boundaries; only loose files at one directory level that still
	// exceed the cap get chunked into numbered parts. <= 0 disables.
	ZipMaxBytes int64
	// EnableZipIndex, when true, uploads a STANDARD-tier
	// `{zipKey}.index.txt` sidecar next to each zip listing its
	// entries. Default: true (set by New()).
	EnableZipIndex bool
	RetryFailed    bool             // re-queue 'failed' rows alongside 'pending'
	Mode           RunMode          // default: RunModeFull
	ScanPaths      []string         // when set with RunModeScan: partial rescan targets
	Now            func() time.Time // injectable clock for tests
	Emit           EventEmitter
}

// Engine owns a run's lifecycle.
type Engine struct {
	opts Options

	// buf is the per-run write buffer that coalesces MarkUploaded and
	// AppendLog writes. Set by runWithID at the start of a run and
	// cleared on exit — Engine is not safe for concurrent runs (the API
	// server's runMu enforces this upstream).
	buf *writeBuffer
}

// New returns an Engine configured with opts. The caller is responsible
// for managing Source / Storage / DB lifetimes.
func New(opts Options) *Engine {
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Emit == nil {
		opts.Emit = DiscardEvents
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = 10
	}
	if opts.ZipThresh <= 0 {
		opts.ZipThresh = 50
	}
	if opts.ZipMaxBytes <= 0 {
		opts.ZipMaxBytes = 2 << 30 // 2 GiB per zip
	}
	return &Engine{opts: opts}
}

// Run drives one full backup cycle: scan -> group pending files ->
// zip or upload -> update DB. Returns the run's DB id; terminal status
// (completed | failed | cancelled) is persisted before returning.
func (e *Engine) Run(ctx context.Context) (int64, error) {
	start := e.opts.Now()
	runID, err := e.opts.DB.CreateRun(ctx, start)
	if err != nil {
		return 0, fmt.Errorf("create run: %w", err)
	}
	return e.runWithID(ctx, runID, start)
}

// RunWithID executes a backup cycle against an already-created run row.
// Used by the HTTP layer so it can return the run id synchronously and
// let the actual work run in a goroutine.
func (e *Engine) RunWithID(ctx context.Context, runID int64) error {
	_, err := e.runWithID(ctx, runID, e.opts.Now())
	return err
}

func (e *Engine) runWithID(ctx context.Context, runID int64, start time.Time) (int64, error) {
	buf := newWriteBuffer(e.opts.DB)
	buf.start(ctx)
	e.buf = buf
	defer func() {
		e.buf = nil
		if err := buf.close(); err != nil {
			slog.Warn("engine write buffer final flush failed", "err", err)
		}
	}()

	e.emit(Event{Type: EventRunStart, RunID: runID, At: start})
	e.log(ctx, runID, db.LogInfo, "run started")

	finalStatus, runErr := e.runInner(ctx, runID)
	finished := e.opts.Now()

	// Bookkeeping always runs to completion: even if the caller's ctx was
	// cancelled we still want the run row finalised and the event emitted.
	// Use a detached context with a short timeout for just these writes.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanupCancel()

	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
		_ = e.opts.DB.AppendLog(cleanupCtx, runID, db.LogError, "run failed: "+errMsg, finished)
	}
	if err := e.opts.DB.FinishRun(cleanupCtx, runID, finalStatus, errMsg, finished); err != nil {
		return runID, fmt.Errorf("finalize run: %w", err)
	}

	stats, _ := e.opts.DB.GetRun(cleanupCtx, runID)
	e.emit(Event{
		Type:  EventRunComplete,
		RunID: runID,
		At:    finished,
		Data: map[string]any{
			"status":         finalStatus,
			"files_scanned":  stats.FilesScanned,
			"files_uploaded": stats.FilesUploaded,
			"bytes_uploaded": stats.BytesUploaded,
			"error_message":  errMsg,
		},
	})
	return runID, runErr
}

func (e *Engine) runInner(ctx context.Context, runID int64) (string, error) {
	mode := e.opts.Mode
	if mode == "" {
		mode = RunModeFull
	}

	// Phase 1: scan (skipped for upload-only runs).
	if mode == RunModeFull || mode == RunModeScan {
		scanStats, err := source.Scan(ctx, e.opts.Source, e.opts.DB, e.opts.ScanPaths, func(msg string) {
			e.log(ctx, runID, db.LogInfo, msg)
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return db.RunCancelled, err
			}
			return db.RunFailed, fmt.Errorf("scan: %w", err)
		}
		if err := e.opts.DB.UpdateRunStats(ctx, runID, scanStats.Seen, 0, 0); err != nil {
			return db.RunFailed, err
		}
		e.emit(Event{
			Type: EventScanComplete, RunID: runID, At: e.opts.Now(),
			Data: map[string]any{
				"seen": scanStats.Seen, "new": scanStats.New,
				"changed": scanStats.Changed, "unchanged": scanStats.Unchanged,
				"missing": scanStats.Missing,
			},
		})
		e.log(ctx, runID, db.LogInfo, fmt.Sprintf(
			"scan: seen=%d new=%d changed=%d missing=%d",
			scanStats.Seen, scanStats.New, scanStats.Changed, scanStats.Missing,
		))
		if mode == RunModeScan {
			e.log(ctx, runID, db.LogInfo, "scan-only mode: skipping upload")
			return db.RunCompleted, nil
		}
	}

	// Phase 2: gather pending files (upload phase).
	pending, err := e.listPending(ctx)
	if err != nil {
		return db.RunFailed, fmt.Errorf("list pending: %w", err)
	}
	if len(pending) == 0 {
		e.log(ctx, runID, db.LogInfo, "no pending files to upload")
		return db.RunCompleted, nil
	}

	groups := GroupFiles(pending, e.opts.ZipThresh, e.opts.ZipMaxBytes)
	e.log(ctx, runID, db.LogInfo, fmt.Sprintf("grouped %d files into %d top-level groups", len(pending), len(groups)))

	if err := os.MkdirAll(e.opts.TmpDir, 0o755); err != nil {
		return db.RunFailed, fmt.Errorf("mkdir tmp: %w", err)
	}

	// 3+4+5. process groups
	var uploaded int64
	var bytesUploaded int64
	zipCounter := 0
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return db.RunCancelled, err
		}
		var (
			up    int64
			bytes int64
		)
		if g.Zip {
			zipCounter++
			up, bytes, err = e.processZipGroup(ctx, runID, g, zipCounter)
		} else {
			up, bytes, err = e.processIndividualGroup(ctx, runID, g)
		}
		uploaded += up
		bytesUploaded += bytes
		_ = e.opts.DB.UpdateRunStats(ctx, runID, int64(len(pending)), uploaded, bytesUploaded)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return db.RunCancelled, err
			}
			return db.RunFailed, err
		}
	}

	return db.RunCompleted, nil
}

func (e *Engine) listPending(ctx context.Context) ([]PendingFile, error) {
	rows, err := e.opts.DB.ListPending(ctx, e.opts.RetryFailed)
	if err != nil {
		return nil, err
	}
	out := make([]PendingFile, 0, len(rows))
	for _, r := range rows {
		out = append(out, PendingFile{ID: r.ID, RelPath: r.Path, Size: r.Size})
	}
	return out, nil
}

func (e *Engine) processZipGroup(ctx context.Context, runID int64, g Group, zipN int) (int64, int64, error) {
	// zipRel mirrors the source directory hierarchy under the key prefix —
	// stored as `files.zip_name` so joinKey(prefix, zip_name) still resolves
	// the full S3 key for sync/restore lookups. The temp file stays flat
	// (basename only) so we don't have to mkdir nested temp subtrees.
	zipRel := ZipRelPath(g.Files, zipN)
	zipBase := path.Base(zipRel)
	zipPath := filepath.Join(e.opts.TmpDir, zipBase)
	defer os.Remove(zipPath)

	e.log(ctx, runID, db.LogInfo, fmt.Sprintf("zipping %d files into %s", len(g.Files), zipRel))
	size, entries, err := CreateZip(ctx, e.opts.Source, g.Files, zipPath)
	if err != nil {
		return 0, 0, fmt.Errorf("create zip %s: %w", zipRel, err)
	}

	md5hex, err := md5File(zipPath)
	if err != nil {
		return 0, 0, err
	}
	key := path.Join(e.opts.KeyPrefix, zipRel)

	e.emit(Event{
		Type: EventUploadStart, RunID: runID, At: e.opts.Now(),
		Data: map[string]any{"key": key, "size": size, "files": len(g.Files)},
	})

	f, err := os.Open(zipPath)
	if err != nil {
		return 0, 0, err
	}
	res, err := e.opts.Storage.Put(ctx, key, f, size)
	f.Close()
	if err != nil {
		e.emit(Event{
			Type: EventUploadFailed, RunID: runID, At: e.opts.Now(),
			Data: map[string]any{"key": key, "error": err.Error()},
		})
		return 0, 0, fmt.Errorf("upload %s: %w", key, err)
	}

	now := e.opts.Now()
	ids := make([]int64, 0, len(g.Files))
	for _, f := range g.Files {
		ids = append(ids, f.ID)
	}
	if err := e.opts.DB.SetZipName(ctx, ids, zipRel); err != nil {
		return 0, 0, fmt.Errorf("set zip name: %w", err)
	}
	if err := e.opts.DB.MarkUploadedBatch(ctx, ids, md5hex, key, now); err != nil {
		return 0, 0, fmt.Errorf("mark uploaded (zip %s): %w", zipRel, err)
	}

	e.log(ctx, runID, db.LogInfo, fmt.Sprintf("uploaded %s (%d bytes, etag=%s)", key, size, res.ETag))
	e.emit(Event{
		Type: EventUploadComplete, RunID: runID, At: now,
		Data: map[string]any{"key": key, "size": size, "etag": res.ETag, "checksum_sha256": res.ChecksumSHA256, "files": len(g.Files)},
	})

	// Upload a plain-text index of the zip's contents to STANDARD so we can
	// list files in a Deep Archive zip without restoring it. A failure here
	// leaves the zip intact — log a warning and move on rather than failing
	// the whole run. Gated so users who don't need remote listing can skip
	// the extra STANDARD-tier objects.
	if e.opts.EnableZipIndex {
		indexKey := key + zipIndexSuffix
		indexBody := strings.Join(entries, "\n") + "\n"
		if _, err := e.opts.Storage.PutStandard(ctx, indexKey, strings.NewReader(indexBody), int64(len(indexBody))); err != nil {
			e.log(ctx, runID, db.LogWarn, fmt.Sprintf("upload zip index %s: %v", indexKey, err))
		} else {
			e.log(ctx, runID, db.LogInfo, fmt.Sprintf("uploaded zip index %s (%d entries)", indexKey, len(entries)))
		}
	}

	return int64(len(g.Files)), size, nil
}

func (e *Engine) processIndividualGroup(ctx context.Context, runID int64, g Group) (int64, int64, error) {
	var uploaded, bytes int64
	for i := 0; i < len(g.Files); i += e.opts.ChunkSize {
		j := i + e.opts.ChunkSize
		if j > len(g.Files) {
			j = len(g.Files)
		}
		for _, pf := range g.Files[i:j] {
			if err := ctx.Err(); err != nil {
				return uploaded, bytes, err
			}
			n, err := e.uploadIndividual(ctx, runID, pf)
			if err != nil {
				return uploaded, bytes, err
			}
			uploaded++
			bytes += n
		}
	}
	return uploaded, bytes, nil
}

func (e *Engine) uploadIndividual(ctx context.Context, runID int64, pf PendingFile) (int64, error) {
	tmp := filepath.Join(e.opts.TmpDir, fmt.Sprintf("ind-%d-%d", runID, pf.ID))
	defer os.Remove(tmp)

	// Copy source -> tmp so we can compute MD5 and then upload from a file.
	size, md5hex, err := copyAndHash(ctx, e.opts.Source, pf.RelPath, tmp)
	if err != nil {
		return 0, fmt.Errorf("copy %s: %w", pf.RelPath, err)
	}

	key := path.Join(e.opts.KeyPrefix, pf.RelPath)
	e.emit(Event{
		Type: EventUploadStart, RunID: runID, At: e.opts.Now(),
		Data: map[string]any{"key": key, "size": size},
	})

	f, err := os.Open(tmp)
	if err != nil {
		return 0, err
	}
	res, err := e.opts.Storage.Put(ctx, key, f, size)
	f.Close()
	if err != nil {
		e.emit(Event{
			Type: EventUploadFailed, RunID: runID, At: e.opts.Now(),
			Data: map[string]any{"key": key, "error": err.Error()},
		})
		if mErr := e.opts.DB.MarkFailed(ctx, pf.ID); mErr != nil {
			return 0, fmt.Errorf("upload %s: %w (and mark failed: %v)", key, err, mErr)
		}
		return 0, fmt.Errorf("upload %s: %w", key, err)
	}

	now := e.opts.Now()
	// Buffered: the flusher goroutine (or writeBuffer.close on run end)
	// commits this in a batch alongside the other individual uploads.
	e.buf.markUploaded(ctx, pf.ID, md5hex, key, now)

	e.emit(Event{
		Type: EventUploadComplete, RunID: runID, At: now,
		Data: map[string]any{"key": key, "size": size, "etag": res.ETag, "checksum_sha256": res.ChecksumSHA256},
	})
	return size, nil
}

func (e *Engine) emit(ev Event) {
	if e.opts.Emit != nil {
		e.opts.Emit(ev)
	}
}

func (e *Engine) log(ctx context.Context, runID int64, level, msg string) {
	if e.buf != nil {
		e.buf.appendLog(ctx, runID, level, msg, e.opts.Now())
		return
	}
	_ = e.opts.DB.AppendLog(ctx, runID, level, msg, e.opts.Now())
}

// copyAndHash copies a source entry to disk, computing md5 on the way.
// Returns the copied size and lowercase hex md5.
func copyAndHash(ctx context.Context, src source.Source, rel, tmp string) (int64, string, error) {
	rc, err := src.Open(ctx, rel)
	if err != nil {
		return 0, "", err
	}
	defer rc.Close()

	out, err := os.Create(tmp)
	if err != nil {
		return 0, "", err
	}
	defer out.Close()

	h := md5.New()
	n, err := io.Copy(io.MultiWriter(out, h), rc)
	if err != nil {
		return n, "", err
	}
	if err := out.Sync(); err != nil {
		return n, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func md5File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
