package engine

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/pathutil"
	"github.com/Wlczak/aws-backup/internal/source"
	"github.com/Wlczak/aws-backup/internal/storage"
)

// ZipIndexSuffix is the suffix appended to a zip's S3 key to form the
// sidecar that lists the archive's contents. STANDARD-tier so listing
// doesn't require a Glacier restore.
const ZipIndexSuffix = ".index.txt"

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
	// MinZipDirFiles is the minimum file count a subdirectory must have
	// to be emitted as its own group during a size-cap split. Subdirs
	// below this threshold are folded into the parent's loose-file pool
	// to avoid producing many tiny zips. <= 0 disables the floor.
	MinZipDirFiles int
	// EnableZipIndex, when true, uploads a STANDARD-tier
	// `{zipKey}.index.txt` sidecar next to each zip listing its
	// entries. Default: true (set by New()).
	EnableZipIndex bool
	RetryFailed    bool             // re-queue 'failed' rows alongside 'pending'
	Mode           RunMode          // default: RunModeFull
	ScanPaths      []string         // when set with RunModeScan: partial rescan targets
	Now            func() time.Time // injectable clock for tests
	Emit           EventEmitter
	// StopRequested, if non-nil, is polled between files / groups during
	// the upload phase. When it returns true the run exits cleanly with
	// RunStopped status (the in-flight upload completes; no further files
	// start). Distinct from context cancellation, which kills mid-stream.
	// (#124)
	StopRequested func() bool
}

// ErrStopRequested is returned by upload helpers when StopRequested
// fires mid-group; the outer loop converts it to db.RunStopped.
var ErrStopRequested = errors.New("engine: stop requested")

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

	e.emit(Event{Type: EventRunStart, RunID: runID, At: start})
	e.log(ctx, runID, db.LogInfo, "run started")

	finalStatus, runErr := e.runInner(ctx, runID)

	// Drain the write buffer BEFORE finalising the run row so the
	// per-file MarkUploaded rows and per-group AppendLog entries land
	// in the DB ahead of FinishRun / EventRunComplete. If we deferred
	// buf.close() (LIFO), runs would briefly observe a 'completed' row
	// with files_uploaded=N while the files table still showed M < N
	// rows uploaded — those (N-M) files would be re-uploaded next run.
	// (#118)
	e.buf = nil
	if err := buf.close(); err != nil {
		slog.Warn("engine write buffer final flush failed", "err", err)
	}

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
	if ferr := e.opts.DB.FinishRun(cleanupCtx, runID, finalStatus, errMsg, finished); ferr != nil {
		// Don't shadow the actual run failure with a finalize-write failure;
		// surface runErr if present and just log the FinishRun problem.
		slog.Warn("finalize run failed", "err", ferr, "run_id", runID)
		if runErr == nil {
			return runID, fmt.Errorf("finalize run: %w", ferr)
		}
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
		scanStats, err := source.Scan(ctx, e.opts.Source, e.opts.DB, e.opts.ScanPaths,
			func(msg string) { e.log(ctx, runID, db.LogInfo, msg) },
			func(p source.ScanProgress) {
				e.emit(Event{
					Type: EventScanProgress, RunID: runID, At: e.opts.Now(),
					Data: map[string]any{
						"seen":    p.Seen,
						"new":     p.New,
						"changed": p.Changed,
					},
				})
			},
		)
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

	// upload-phase helper: reclassify ctx cancellation as RunCancelled
	// instead of RunFailed, so a user-initiated cancel during any of
	// these blocking S3 / DB / mkdir calls produces the right run-status.
	// (#119)
	classify := func(stage string, err error) (string, error) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return db.RunCancelled, err
		}
		return db.RunFailed, fmt.Errorf("%s: %w", stage, err)
	}

	// Phase 2: list S3 once — reused for reconciliation and counter seeding.
	// Normalize the prefix so List does not match sibling keys (e.g. a
	// configured prefix "backups" must not capture "backups2/...").
	s3Keys, err := e.opts.Storage.List(ctx, pathutil.NormalizeS3ListPrefix(e.opts.KeyPrefix))
	if err != nil {
		return classify("list s3 keys", err)
	}

	// Reconcile DB against S3: any zip uploaded in a prior (partially-failed)
	// run has an index sidecar. Files listed there but still pending or zipped
	// in the DB are marked uploaded so listPending excludes them.
	if err := e.reconcileFromS3(ctx, runID, s3Keys); err != nil {
		return classify("reconcile from S3", err)
	}

	// Phase 3: gather pending files (upload phase).
	pending, err := e.listPending(ctx)
	if err != nil {
		return classify("list pending", err)
	}
	if len(pending) == 0 {
		e.log(ctx, runID, db.LogInfo, "no pending files to upload")
		return db.RunCompleted, nil
	}

	groups := GroupFiles(pending, e.opts.ZipThresh, e.opts.MinZipDirFiles, e.opts.ZipMaxBytes)
	e.log(ctx, runID, db.LogInfo, fmt.Sprintf("grouped %d files into %d top-level groups", len(pending), len(groups)))

	// Surface the planned upload size up front so the UI can render an
	// accurate progress denominator instead of "n-1 / n" that only fills
	// as each upload starts. (#126)
	var totalBytes int64
	for _, pf := range pending {
		totalBytes += pf.Size
	}
	e.emit(Event{
		Type: EventUploadPlan, RunID: runID, At: e.opts.Now(),
		Data: map[string]any{
			"total_files":  len(pending),
			"total_groups": len(groups),
			"total_bytes":  totalBytes,
		},
	})

	if err := os.MkdirAll(e.opts.TmpDir, 0o755); err != nil {
		return classify("mkdir tmp", err)
	}

	// Seed per-directory zip counters so new zips continue the sequence
	// (_2, _3, …) instead of restarting at _1 and silently overwriting the
	// previous archive. We consult both the DB and S3 and take the max, so
	// the counter survives DB corruption or a full wipe.
	dirMaxN := map[string]int{}
	seedDirMaxN := func(zipRelPath string) {
		dir := path.Dir(zipRelPath)
		if dir == "." {
			dir = ""
		}
		if n := parseZipNumber(zipRelPath); n > dirMaxN[dir] {
			dirMaxN[dir] = n
		}
	}

	existingZips, err := e.opts.DB.ListZipNames(ctx)
	if err != nil {
		return classify("list zip names", err)
	}
	for _, z := range existingZips {
		seedDirMaxN(z)
	}

	// Also scan S3 — ground truth when the DB is stale or empty.
	prefix := e.opts.KeyPrefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	for _, k := range s3Keys {
		if !strings.HasSuffix(k, ".zip") {
			continue
		}
		rel := strings.TrimPrefix(k, prefix)
		seedDirMaxN(rel)
	}

	var (
		uploaded, bytesUploaded int64
		groupErrCount           int
	)
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return db.RunCancelled, err
		}
		if e.opts.StopRequested != nil && e.opts.StopRequested() {
			e.log(ctx, runID, db.LogInfo, "stop requested — finishing run after current group")
			return db.RunStopped, nil
		}
		var up, bytes int64
		if g.Zip {
			dir := commonDirPath(g.Files)
			// On ErrAlreadyExists from S3 (IfNoneMatch=* tripped), bump
			// the counter and retry once with a fresh slot. Cap the
			// retry budget so a perpetually-occupied dir doesn't loop.
			// (#116)
			for attempt := 0; attempt < 5; attempt++ {
				dirMaxN[dir]++
				up, bytes, err = e.processZipGroup(ctx, runID, g, dirMaxN[dir])
				if !errors.Is(err, storage.ErrAlreadyExists) {
					break
				}
				e.log(ctx, runID, db.LogWarn, fmt.Sprintf("zip key collision at slot %d for dir %q, advancing", dirMaxN[dir], dir))
			}
		} else {
			up, bytes, err = e.processIndividualGroup(ctx, runID, g)
		}
		uploaded += up
		bytesUploaded += bytes
		if uerr := e.opts.DB.UpdateUploadStats(ctx, runID, uploaded, bytesUploaded); uerr != nil {
			slog.Warn("update run stats failed", "err", uerr, "run_id", runID)
		}
		if err != nil {
			if errors.Is(err, ErrStopRequested) {
				e.log(ctx, runID, db.LogInfo, "stop requested — exiting after in-flight uploads")
				return db.RunStopped, nil
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return db.RunCancelled, err
			}
			// One failed group should not abort the rest of the run; the
			// affected files stay pending/failed in the DB and will be
			// retried next run. Surface true infrastructure failure only
			// when every group fails.
			groupErrCount++
			e.log(ctx, runID, db.LogError, fmt.Sprintf("group failed: %v", err))
		}
	}

	if groupErrCount > 0 && groupErrCount == len(groups) {
		return db.RunFailed, fmt.Errorf("all %d groups failed", groupErrCount)
	}
	return db.RunCompleted, nil
}

// reconcileFromS3 reads every zip index sidecar in S3 and, for each one
// whose corresponding .zip object is also present, updates any DB rows that
// are still pending/zipped/failed to uploaded. This recovers from the crash
// window between a successful S3 put and the subsequent DB commit.
func (e *Engine) reconcileFromS3(ctx context.Context, runID int64, s3Keys []string) error {
	prefix := e.opts.KeyPrefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	zipSet := make(map[string]struct{}, len(s3Keys))
	for _, k := range s3Keys {
		if strings.HasSuffix(k, ".zip") {
			zipSet[k] = struct{}{}
		}
	}

	var total int64
	for _, k := range s3Keys {
		if !strings.HasSuffix(k, ZipIndexSuffix) {
			continue
		}
		// Cancellation aborts the loop cleanly; transient cancels
		// during a Get are handled below.
		if err := ctx.Err(); err != nil {
			return err
		}
		zipKey := strings.TrimSuffix(k, ZipIndexSuffix)
		if _, ok := zipSet[zipKey]; !ok {
			// Orphan sidecar: zip upload failed after the sidecar
			// succeeded. Delete it now so flaky-network deployments
			// don't accumulate dangling .index.txt keys monotonically
			// across runs. Best-effort: a delete failure just defers
			// cleanup to a future run. (#121)
			e.log(ctx, runID, db.LogWarn, fmt.Sprintf("reconcile: index %s has no matching zip, deleting orphan", k))
			if delErr := e.opts.Storage.Delete(ctx, k); delErr != nil {
				e.log(ctx, runID, db.LogWarn, fmt.Sprintf("reconcile: orphan delete %s failed: %v", k, delErr))
			}
			continue
		}
		paths, err := e.readIndexPaths(ctx, k)
		if err != nil {
			// Cancellation must abort the run; everything else is a
			// per-sidecar issue (transient 5xx, corrupt object) — log
			// and continue so one bad sidecar doesn't wedge every
			// subsequent backup. (#120)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			e.log(ctx, runID, db.LogWarn, fmt.Sprintf("reconcile: read %s failed, skipping: %v", k, err))
			continue
		}
		if len(paths) == 0 {
			continue
		}
		zipRel := strings.TrimPrefix(zipKey, prefix)
		n, err := e.opts.DB.ReconcileZip(ctx, paths, zipRel, zipKey, e.opts.Now())
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			e.log(ctx, runID, db.LogWarn, fmt.Sprintf("reconcile: db for %s failed, skipping: %v", zipKey, err))
			continue
		}
		if n > 0 {
			total += n
			e.log(ctx, runID, db.LogInfo, fmt.Sprintf("reconcile: marked %d files uploaded from %s", n, zipKey))
		}
	}
	if total > 0 {
		e.log(ctx, runID, db.LogInfo, fmt.Sprintf("reconcile: %d files total recovered from S3 state", total))
	}
	return nil
}

func (e *Engine) readIndexPaths(ctx context.Context, indexKey string) ([]string, error) {
	rc, err := e.opts.Storage.Get(ctx, indexKey)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var paths []string
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, sc.Err()
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

	key := path.Join(e.opts.KeyPrefix, zipRel)
	indexKey := key + ZipIndexSuffix

	// Sum source bytes up front so copy_progress events can render a
	// real percent. The resulting tmp zip is typically smaller than
	// this total due to compression, but for *copy* progress we report
	// against what's being read off the source. (#127)
	var groupTotalBytes int64
	for _, f := range g.Files {
		groupTotalBytes += f.Size
	}

	if err := ensureTmpSpace(e.opts.TmpDir, groupTotalBytes); err != nil {
		return 0, 0, fmt.Errorf("zip group %s: %w", zipRel, err)
	}

	e.log(ctx, runID, db.LogInfo, fmt.Sprintf("zipping %d files into %s", len(g.Files), zipRel))
	size, entries, err := CreateZip(ctx, e.opts.Source, g.Files, zipPath, e.copyWrapZip(runID, key, groupTotalBytes))
	if err != nil {
		return 0, 0, fmt.Errorf("create zip %s: %w", zipRel, err)
	}
	e.emitCopyProgress(runID, key, groupTotalBytes, groupTotalBytes) // belt-and-braces final 100%

	md5hex, err := md5File(zipPath)
	if err != nil {
		return 0, 0, err
	}
	zipSHA256, err := sha256File(zipPath)
	if err != nil {
		return 0, 0, err
	}

	e.emit(Event{
		Type: EventUploadStart, RunID: runID, At: e.opts.Now(),
		Data: map[string]any{"key": key, "size": size, "files": len(g.Files)},
	})

	// Upload the STANDARD-tier index sidecar BEFORE the zip itself so a
	// crash between the two uploads can be recovered: the next run's
	// reconcileFromS3 reads the sidecar to mark files uploaded. Doing it
	// in the reverse order (the previous behaviour) could leave an
	// orphaned DEEP_ARCHIVE zip with no DB linkage and no recovery path,
	// causing the next run to re-zip the same files under a new key.
	indexUploaded := false
	if e.opts.EnableZipIndex {
		indexBody := strings.Join(entries, "\n") + "\n"
		indexSum := sha256.Sum256([]byte(indexBody))
		indexSHA256 := hex.EncodeToString(indexSum[:])
		if e.skipIfMatches(ctx, indexKey, indexSHA256) {
			e.log(ctx, runID, db.LogInfo, fmt.Sprintf("skip zip index %s: SHA256 matches existing object", indexKey))
			indexUploaded = true
		} else {
			if _, err := e.opts.Storage.PutStandard(ctx, indexKey, strings.NewReader(indexBody), int64(len(indexBody))); err != nil {
				e.emit(Event{
					Type: EventUploadFailed, RunID: runID, At: e.opts.Now(),
					Data: map[string]any{"key": indexKey, "error": err.Error()},
				})
				return 0, 0, fmt.Errorf("upload zip index %s: %w", indexKey, err)
			}
			indexUploaded = true
			e.log(ctx, runID, db.LogInfo, fmt.Sprintf("uploaded zip index %s (%d entries)", indexKey, len(entries)))
		}
	}

	now := e.opts.Now()
	ids := make([]int64, 0, len(g.Files))
	for _, f := range g.Files {
		ids = append(ids, f.ID)
	}

	// Dedup: if S3 already holds the zip with the same SHA256, skip the
	// PutIfAbsent entirely — saves a redundant version on a versioned
	// bucket. A *different* SHA256 at this key still falls through to
	// PutIfAbsent, which returns ErrAlreadyExists so the caller advances
	// to the next counter slot exactly as before. (#133, #116)
	if e.skipIfMatches(ctx, key, zipSHA256) {
		e.log(ctx, runID, db.LogInfo, fmt.Sprintf("skip zip upload %s: SHA256 matches existing object", key))
		if err := e.opts.DB.MarkZipUploadedBatch(ctx, ids, zipRel, md5hex, key, now); err != nil {
			return 0, 0, fmt.Errorf("mark uploaded (zip %s): %w", zipRel, err)
		}
		e.emit(Event{
			Type: EventUploadComplete, RunID: runID, At: now,
			Data: map[string]any{"key": key, "size": size, "checksum_sha256": zipSHA256, "files": len(g.Files), "skipped": true},
		})
		return int64(len(g.Files)), size, nil
	}

	f, err := os.Open(zipPath)
	if err != nil {
		return 0, 0, err
	}
	// PutIfAbsent so a retry under the same key can't silently overwrite
	// a prior DEEP_ARCHIVE object whose content may differ. The caller
	// catches ErrAlreadyExists and advances to the next counter slot.
	// (#116)
	res, err := e.opts.Storage.PutIfAbsent(e.uploadProgressCtx(ctx, runID, key, size), key, f, size)
	f.Close()
	if err != nil {
		e.emit(Event{
			Type: EventUploadFailed, RunID: runID, At: e.opts.Now(),
			Data: map[string]any{"key": key, "error": err.Error()},
		})
		// Best-effort: remove the orphaned sidecar so a half-uploaded group
		// doesn't leave a dangling .index.txt pointing at a missing zip.
		if indexUploaded {
			if delErr := e.opts.Storage.Delete(ctx, indexKey); delErr != nil {
				e.log(ctx, runID, db.LogWarn, fmt.Sprintf("cleanup orphan index %s: %v", indexKey, delErr))
			}
		}
		return 0, 0, fmt.Errorf("upload %s: %w", key, err)
	}

	// Single transactional write so a partial failure can't leave files
	// stuck in the intermediate 'zipped' state with no md5/s3_key.
	if err := e.opts.DB.MarkZipUploadedBatch(ctx, ids, zipRel, md5hex, key, now); err != nil {
		return 0, 0, fmt.Errorf("mark uploaded (zip %s): %w", zipRel, err)
	}

	e.log(ctx, runID, db.LogInfo, fmt.Sprintf("uploaded %s (%d bytes, etag=%s)", key, size, res.ETag))
	e.emit(Event{
		Type: EventUploadComplete, RunID: runID, At: now,
		Data: map[string]any{"key": key, "size": size, "etag": res.ETag, "checksum_sha256": res.ChecksumSHA256, "files": len(g.Files)},
	})

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
			if e.opts.StopRequested != nil && e.opts.StopRequested() {
				return uploaded, bytes, ErrStopRequested
			}
			n, err := e.uploadIndividual(ctx, runID, pf)
			if err != nil {
				// Cancellation aborts the whole run; otherwise the row
				// is already marked failed by uploadIndividual — log
				// and keep going so one bad file doesn't stop the rest.
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return uploaded, bytes, err
				}
				e.log(ctx, runID, db.LogWarn, fmt.Sprintf("skip %s after upload error: %v", pf.RelPath, err))
				continue
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

	key := path.Join(e.opts.KeyPrefix, pf.RelPath)

	if err := ensureTmpSpace(e.opts.TmpDir, pf.Size); err != nil {
		return 0, fmt.Errorf("copy %s: %w", pf.RelPath, err)
	}

	// Copy source -> tmp so we can compute MD5 and then upload from a file.
	// Wrap the source reader so bytes flowing through emit copy_progress
	// events; key matches the upload phase so the UI keeps one entry per
	// item with a phase field.
	size, md5hex, sha256hex, err := copyAndHash(ctx, e.opts.Source, pf.RelPath, tmp, e.copyWrap(runID, key, pf.Size))
	if err != nil {
		return 0, fmt.Errorf("copy %s: %w", pf.RelPath, err)
	}
	e.emitCopyProgress(runID, key, size, size) // belt-and-braces final 100% sample

	// Dedup: if S3 already holds an object at this key whose SHA256
	// matches the local content, skip the upload to avoid creating a
	// redundant new version on a versioned bucket. (#133)
	if e.skipIfMatches(ctx, key, sha256hex) {
		e.log(ctx, runID, db.LogInfo, fmt.Sprintf("skip upload %s: SHA256 matches existing object", key))
		now := e.opts.Now()
		e.buf.markUploaded(ctx, pf.ID, md5hex, key, now)
		e.emit(Event{
			Type: EventUploadComplete, RunID: runID, At: now,
			Data: map[string]any{"key": key, "size": size, "checksum_sha256": sha256hex, "skipped": true},
		})
		return size, nil
	}

	e.emit(Event{
		Type: EventUploadStart, RunID: runID, At: e.opts.Now(),
		Data: map[string]any{"key": key, "size": size},
	})

	f, err := os.Open(tmp)
	if err != nil {
		return 0, err
	}
	res, err := e.opts.Storage.Put(e.uploadProgressCtx(ctx, runID, key, size), key, f, size)
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

// emitCopyProgress publishes an EventCopyProgress sample for key.
// Used both via the throttled progressReader callback during copies
// and as the explicit final 100% sample after the copy returns.
func (e *Engine) emitCopyProgress(runID int64, key string, read, total int64) {
	var pct float64
	if total > 0 {
		pct = float64(read) / float64(total) * 100
		if pct > 100 {
			pct = 100
		}
	}
	e.emit(Event{
		Type: EventCopyProgress, RunID: runID, At: e.opts.Now(),
		Data: map[string]any{
			"key":          key,
			"bytes_copied": read,
			"size":         total,
			"percent":      pct,
		},
	})
}

// copyWrap returns an io.Reader-wrap closure for a single source file,
// emitting throttled EventCopyProgress events keyed by S3 key. Returns
// nil when the engine has no event sink, so copyAndHash skips the
// allocation entirely. (#127)
func (e *Engine) copyWrap(runID int64, key string, size int64) func(io.Reader) io.Reader {
	if e.opts.Emit == nil {
		return nil
	}
	return func(r io.Reader) io.Reader {
		return newProgressReader(r, size, defaultProgressInterval, func(read, total int64) {
			e.emitCopyProgress(runID, key, read, total)
		})
	}
}

// copyWrapZip returns a wrap closure for the zip-group copy path.
// Bytes from every per-file reader feed one shared running total
// against the group's full source byte count (sum of f.Size); throttling
// is enforced here, not per-reader, so file boundaries don't reset
// the throttle and flood the bus. The returned closure is intended
// to be passed to CreateZip and applied once per zip entry. (#127)
func (e *Engine) copyWrapZip(runID int64, key string, total int64) func(io.Reader) io.Reader {
	if e.opts.Emit == nil {
		return nil
	}
	var (
		seen     int64
		lastEmit time.Time
	)
	return func(r io.Reader) io.Reader {
		return &counterReader{r: r, onRead: func(n int) {
			seen += int64(n)
			now := e.opts.Now()
			if !lastEmit.IsZero() && now.Sub(lastEmit) < defaultProgressInterval {
				return
			}
			lastEmit = now
			e.emitCopyProgress(runID, key, seen, total)
		}}
	}
}

// emitUploadProgress publishes an EventUploadProgress sample for key.
// Mirrors emitCopyProgress so consumers see a consistent payload shape.
func (e *Engine) emitUploadProgress(runID int64, key string, read, total int64) {
	var pct float64
	if total > 0 {
		pct = float64(read) / float64(total) * 100
		if pct > 100 {
			pct = 100
		}
	}
	e.emit(Event{
		Type: EventUploadProgress, RunID: runID, At: e.opts.Now(),
		Data: map[string]any{
			"key":            key,
			"bytes_uploaded": read,
			"size":           total,
			"percent":        pct,
		},
	})
}

// uploadProgressCtx returns a context that, when passed to Storage.Put*,
// produces upload_progress events at HTTP-frame granularity. The S3
// client's transport (installed by NewS3Storage) wraps each request body
// in a counting reader keyed off the context callback. Multipart parts
// run concurrently, so we use atomic counters; emits are CAS-throttled
// to one per defaultProgressInterval to avoid flooding the bus.
func (e *Engine) uploadProgressCtx(ctx context.Context, runID int64, key string, size int64) context.Context {
	if e.opts.Emit == nil {
		return ctx
	}
	var sent atomic.Int64
	var lastEmitNs atomic.Int64
	return storage.WithUploadProgress(ctx, func(n int64) {
		total := sent.Add(n)
		nowNs := e.opts.Now().UnixNano()
		isFinal := size > 0 && total >= size
		last := lastEmitNs.Load()
		if !isFinal && last != 0 && nowNs-last < int64(defaultProgressInterval) {
			return
		}
		if !lastEmitNs.CompareAndSwap(last, nowNs) {
			return
		}
		e.emitUploadProgress(runID, key, total, size)
	})
}

func (e *Engine) log(ctx context.Context, runID int64, level, msg string) {
	if e.buf != nil {
		e.buf.appendLog(ctx, runID, level, msg, e.opts.Now())
		return
	}
	_ = e.opts.DB.AppendLog(ctx, runID, level, msg, e.opts.Now())
}

// copyAndHash copies a source entry to disk, computing both md5 and
// sha256 on the way. md5 is what we persist in the DB for legacy
// reasons; sha256 is used transiently by the dedup check that skips
// the upload when S3 already holds an object with the same SHA256.
// (#133) wrap, if non-nil, wraps the source reader before bytes flow
// into the writer — the engine uses this to inject a progressReader
// that emits live copy_progress events for slow / large source reads.
func copyAndHash(ctx context.Context, src source.Source, rel, tmp string, wrap func(io.Reader) io.Reader) (int64, string, string, error) {
	rc, err := src.Open(ctx, rel)
	if err != nil {
		return 0, "", "", err
	}
	defer rc.Close()

	out, err := os.Create(tmp)
	if err != nil {
		return 0, "", "", err
	}
	defer out.Close()

	var reader io.Reader = rc
	if wrap != nil {
		reader = wrap(rc)
	}

	hMD5 := md5.New()
	hSHA := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hMD5, hSHA), reader)
	if err != nil {
		return n, "", "", err
	}
	if err := out.Sync(); err != nil {
		return n, "", "", err
	}
	return n, hex.EncodeToString(hMD5.Sum(nil)), hex.EncodeToString(hSHA.Sum(nil)), nil
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

// skipIfMatches reports whether S3 already holds an object at key whose
// ChecksumSHA256 equals sha256hex — the signal callers use to skip a
// byte-identical re-upload. Any other outcome (object missing, checksum
// unknown because the backend didn't echo one, content differs, transient
// HEAD error) returns false so the regular upload path runs and the
// upload itself surfaces any genuine network problem. (#133)
func (e *Engine) skipIfMatches(ctx context.Context, key, sha256hex string) bool {
	if sha256hex == "" {
		return false
	}
	h, err := e.opts.Storage.Head(ctx, key)
	if err != nil {
		return false
	}
	return h.ChecksumSHA256 != "" && h.ChecksumSHA256 == sha256hex
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
