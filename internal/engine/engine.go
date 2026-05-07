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
	"strconv"
	"strings"
	"sync"
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
	// exceed the cap get chunked into numbered parts. 0 = use default
	// of 2 GiB; set to a negative value to disable the cap entirely.
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
	// CopyThreads is the number of concurrent staging workers
	// (source → tmp). 0 or 1 = sequential.
	CopyThreads int
	// UploadThreads is the number of concurrent S3 upload workers.
	// 0 or 1 = sequential.
	UploadThreads int
	// PipelineQueue bounds how many staged groups wait between the copy
	// and upload stages. 0 = auto (max(UploadThreads, 1)).
	PipelineQueue int
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
	if opts.ZipMaxBytes == 0 {
		opts.ZipMaxBytes = 2 << 30 // 2 GiB per zip
	}
	if opts.CopyThreads <= 0 {
		opts.CopyThreads = 1
	}
	if opts.UploadThreads <= 0 {
		opts.UploadThreads = 1
	}
	if opts.PipelineQueue <= 0 {
		q := opts.UploadThreads
		if q < 1 {
			q = 1
		}
		opts.PipelineQueue = q
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
		// GORM's transaction-rollback errors stringify ctx.Canceled in a
		// way that doesn't unwrap, so a caller's
		// errors.Is(err, context.Canceled) check would miss the cancel
		// signal. Surface ctx.Err() directly when it's the actual cause.
		if cerr := ctx.Err(); cerr != nil {
			return 0, cerr
		}
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
			// SQLite returns "interrupted (9)" when GORM passes a
			// cancelled ctx to an in-flight statement; that error
			// doesn't unwrap to context.Canceled, so a user cancel
			// would be misclassified as RunFailed without this check.
			// (#155)
			if cerr := ctx.Err(); cerr != nil {
				return db.RunCancelled, cerr
			}
			return db.RunFailed, fmt.Errorf("scan: %w", err)
		}
		if err := e.opts.DB.UpdateRunStats(ctx, runID, scanStats.Seen, 0, 0); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return db.RunCancelled, cerr
			}
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
		// SQLite returns "interrupted (9)" when GORM passes a
		// cancelled ctx to an in-flight statement; that error
		// doesn't unwrap to context.Canceled, so a user cancel
		// would be misclassified as RunFailed without this check.
		// (#155)
		if cerr := ctx.Err(); cerr != nil {
			return db.RunCancelled, cerr
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

	// Sweep tmp dir of orphan ind-N files whose IDs aren't pending any
	// more (e.g. the row got marked uploaded by reconcileFromS3 between
	// runs, or the file was removed from the source). Without this
	// sweep, every "leave-tmp-on-failure" path could leak indefinitely
	// across many runs. Cheap: one ReadDir + a few unlinks. (#127)
	keepIDs := make(map[int64]struct{}, len(pending))
	for _, pf := range pending {
		keepIDs[pf.ID] = struct{}{}
	}
	e.sweepOrphanTmps(ctx, runID, e.opts.TmpDir, keepIDs)

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

	return e.runPipeline(ctx, runID, groups, dirMaxN)
}

// runPipeline drives the upload phase with a copy-worker pool feeding a
// bounded queue into an upload-worker pool. At CopyThreads=1 /
// UploadThreads=1 this is equivalent to the old sequential loop.
//
// Pipeline topology:
//
//	groups → workCh → [copy workers] → stagedCh → [upload workers] → resultCh
//
// The orchestrator feeds workCh, collects from resultCh, re-feeds collision
// retries, and signals stop/cancel to both worker pools via ctx.
func (e *Engine) runPipeline(ctx context.Context, runID int64, groups []Group, dirMaxN map[string]int) (string, error) {
	if len(groups) == 0 {
		return db.RunCompleted, nil
	}

	ct := e.opts.CopyThreads
	ut := e.opts.UploadThreads
	pq := e.opts.PipelineQueue

	var dirMu sync.Mutex

	// workItem wraps a Group with retry metadata.
	type workItem struct {
		group    Group
		attempts int // how many upload attempts have been made for this group
	}

	// uploadResult is what upload workers report back to the orchestrator.
	type uploadResult struct {
		files int64
		bytes int64
		err   error
		// retryGroup is non-nil when the upload worker wants the orchestrator
		// to re-queue this group for a fresh copy+upload attempt.
		retryGroup *workItem
	}

	// workCh pre-seeded with all groups; orchestrator re-feeds retries.
	// Retries fit because each retry is enqueued only after the previous
	// attempt's result was consumed, freeing capacity.
	workCh   := make(chan workItem, len(groups))
	stagedCh := make(chan stagedItem, pq)
	// resultCh must hold results from all copy + upload workers in the
	// worst case (all fail simultaneously) to prevent goroutine leaks.
	resultCh := make(chan uploadResult, ct+ut)

	// inflight counts groups anywhere in the pipeline (dequeued from workCh
	// but result not yet received by the orchestrator).
	var inflight int64

	// seed workCh
	for _, g := range groups {
		workCh <- workItem{group: g}
		inflight++
	}

	// stopSending is closed by the orchestrator to signal copy workers to
	// skip further staging (they still report a result per item so inflight
	// reaches 0 and workCh gets closed cleanly).
	stopSending := make(chan struct{})
	stopOnce := sync.Once{}
	haltSubmission := func() { stopOnce.Do(func() { close(stopSending) }) }

	// --- copy workers ---
	var copyWg sync.WaitGroup
	copyWg.Add(ct)
	for i := 0; i < ct; i++ {
		go func() {
			defer copyWg.Done()
			for item := range workCh {
				// Fast-path: if stopping or ctx cancelled, skip staging.
				select {
				case <-stopSending:
					resultCh <- uploadResult{err: ErrStopRequested}
					continue
				case <-ctx.Done():
					resultCh <- uploadResult{err: ctx.Err()}
					continue
				default:
				}

				staged, err := e.stageGroup(ctx, runID, item.group, dirMaxN, &dirMu)
				if err != nil {
					resultCh <- uploadResult{err: err}
					continue
				}
				staged.attempts = item.attempts
				// Push to upload queue, respecting stop/cancel so we
				// don't block forever with a staged tmp file on disk.
				select {
				case stagedCh <- staged:
				case <-stopSending:
					// Zips have no resume path so the staged zip can go;
					// individual ind-* tmps are deliberately preserved so
					// the next run's tryReuseTmp can skip the source read. (#127, #237)
					os.Remove(staged.zipTmpPath)
					resultCh <- uploadResult{err: ErrStopRequested}
				case <-ctx.Done():
					os.Remove(staged.zipTmpPath)
					resultCh <- uploadResult{err: ctx.Err()}
				}
			}
		}()
	}

	// Close stagedCh once all copy workers exit.
	go func() {
		copyWg.Wait()
		close(stagedCh)
	}()

	// --- upload workers ---
	var uploadWg sync.WaitGroup
	uploadWg.Add(ut)
	for i := 0; i < ut; i++ {
		go func() {
			defer uploadWg.Done()
			for item := range stagedCh {
				if err := ctx.Err(); err != nil {
					// Drop the staged zip (no resume path) but keep the
					// individual ind-* tmps so the next run's tryReuseTmp
					// can skip re-reading the source. (#127, #237)
					os.Remove(item.zipTmpPath)
					resultCh <- uploadResult{err: err}
					continue
				}
				files, bytes, err := e.uploadStaged(ctx, runID, item)
				if errors.Is(err, storage.ErrAlreadyExists) && item.attempts < 4 {
					// Key collision — re-queue for a fresh copy with the next slot.
					resultCh <- uploadResult{retryGroup: &workItem{group: item.group, attempts: item.attempts + 1}}
					continue
				}
				resultCh <- uploadResult{files: files, bytes: bytes, err: err}
			}
		}()
	}

	// Close resultCh once all upload workers exit.
	go func() {
		uploadWg.Wait()
		close(resultCh)
	}()

	// --- orchestrator ---
	var (
		uploaded, bytesUploaded int64
		groupErrCount           int
		terminal                = db.RunCompleted
		terminalErr             error
		stopping                bool
	)

	for res := range resultCh {
		// Handle retry request from upload worker (zip key collision).
		if res.retryGroup != nil {
			rg := res.retryGroup
			dir := commonDirPath(rg.group.Files)
			e.log(ctx, runID, db.LogWarn, fmt.Sprintf("zip key collision for dir %q, re-queuing (attempt %d)", dir, rg.attempts+1))
			if !stopping {
				inflight++ // re-counts the group
				workCh <- *rg
			} else {
				// Can't re-queue during stop; count as error and log
				// loudly so operators can see which group was abandoned
				// rather than just watching groupErrCount tick up
				// silently. (#169)
				e.log(ctx, runID, db.LogWarn, fmt.Sprintf(
					"abandoned retry due to stop request: dir=%q attempts=%d files=%d",
					dir, rg.attempts+1, len(rg.group.Files),
				))
				groupErrCount++
			}
			// This result consumed one inflight slot; the re-queued item adds another.
			inflight--
			if inflight == 0 {
				// All groups done (edge case: only retried groups, all declined).
				haltSubmission()
				close(workCh)
			}
			continue
		}

		uploaded += res.files
		bytesUploaded += res.bytes
		if uerr := e.opts.DB.UpdateUploadStats(ctx, runID, uploaded, bytesUploaded); uerr != nil {
			slog.Warn("update run stats failed", "err", uerr, "run_id", runID)
		}

		if res.err != nil {
			if errors.Is(res.err, ErrStopRequested) {
				if terminal == db.RunCompleted {
					terminal = db.RunStopped
					terminalErr = nil
				}
			} else if errors.Is(res.err, context.Canceled) || errors.Is(res.err, context.DeadlineExceeded) {
				terminal = db.RunCancelled
				terminalErr = res.err
			} else {
				groupErrCount++
				e.log(ctx, runID, db.LogError, fmt.Sprintf("group failed: %v", res.err))
			}
		}

		inflight--
		if inflight == 0 {
			// All originally-submitted groups (plus any retries) have finished.
			haltSubmission()
			close(workCh)
		}

		// Poll stop/cancel between results.
		if !stopping {
			if terminal == db.RunCancelled || terminal == db.RunStopped {
				stopping = true
				haltSubmission()
			} else if e.opts.StopRequested != nil && e.opts.StopRequested() {
				stopping = true
				terminal = db.RunStopped
				haltSubmission()
				e.log(ctx, runID, db.LogInfo, "stop requested — draining in-flight uploads")
			} else if err := ctx.Err(); err != nil {
				stopping = true
				terminal = db.RunCancelled
				terminalErr = err
				haltSubmission()
			}
		}
	}

	if terminal == db.RunCompleted && groupErrCount > 0 && groupErrCount == len(groups) {
		return db.RunFailed, fmt.Errorf("all %d groups failed", groupErrCount)
	}
	if terminal == db.RunStopped {
		e.log(ctx, runID, db.LogInfo, "stop requested — exiting after in-flight uploads")
	}
	return terminal, terminalErr
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
			// Skip the delete on cancellation: the failure is just the
			// cancel surfacing through the SDK and logging it as an
			// orphan-delete error is misleading. The next run will pick
			// the sidecar up. (#170)
			if ctx.Err() != nil {
				return ctx.Err()
			}
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
		out = append(out, PendingFile{ID: r.ID, RelPath: r.Path, Size: r.Size, MTime: r.MTime})
	}
	return out, nil
}

// stagedItem is a group that has finished its source→tmp copy phase and is
// queued for S3 upload.
type stagedItem struct {
	group   Group
	isZip   bool
	attempts int // for retry budget tracking

	// Zip fields.
	zipTmpPath string
	zipKey     string
	indexKey   string
	zipRel     string
	zipSize    int64
	zipMD5hex  string
	zipSHA256  string
	zipEntries []string

	// Individual-file fields.
	individuals []stagedIndividual
}

type stagedIndividual struct {
	pf        PendingFile
	tmpPath   string
	key       string
	size      int64
	md5hex    string
	sha256hex string
}

// claimZipSlot atomically advances and returns the next counter for dir.
func claimZipSlot(dir string, dirMaxN map[string]int, mu *sync.Mutex) int {
	mu.Lock()
	defer mu.Unlock()
	dirMaxN[dir]++
	return dirMaxN[dir]
}

// stageGroup copies a group's source files to tmp, returning a stagedItem
// ready for upload. For zip groups, CreateZip is called. For individual
// groups, each file is copyAndHash'd into its own tmp file.
func (e *Engine) stageGroup(ctx context.Context, runID int64, g Group, dirMaxN map[string]int, mu *sync.Mutex) (stagedItem, error) {
	if g.Zip {
		dir := commonDirPath(g.Files)
		zipN := claimZipSlot(dir, dirMaxN, mu)
		return e.stageZipGroup(ctx, runID, g, zipN)
	}
	return e.stageIndividualGroup(ctx, runID, g)
}

// stageZipGroup copies source files into a single zip in tmp.
func (e *Engine) stageZipGroup(ctx context.Context, runID int64, g Group, zipN int) (stagedItem, error) {
	zipRel := ZipRelPath(g.Files, zipN)
	zipBase := path.Base(zipRel)
	zipPath := filepath.Join(e.opts.TmpDir, zipBase)

	key := path.Join(e.opts.KeyPrefix, zipRel)
	indexKey := key + ZipIndexSuffix

	var groupTotalBytes int64
	for _, f := range g.Files {
		groupTotalBytes += f.Size
	}

	if err := ensureTmpSpace(e.opts.TmpDir, groupTotalBytes); err != nil {
		return stagedItem{}, fmt.Errorf("zip group %s: %w", zipRel, err)
	}

	e.log(ctx, runID, db.LogInfo, fmt.Sprintf("zipping %d files into %s", len(g.Files), zipRel))
	size, entries, err := CreateZip(ctx, e.opts.Source, g.Files, zipPath, e.copyWrapZip(runID, key, groupTotalBytes))
	if err != nil {
		os.Remove(zipPath)
		return stagedItem{}, fmt.Errorf("create zip %s: %w", zipRel, err)
	}
	e.emitCopyProgress(runID, key, groupTotalBytes, groupTotalBytes)

	md5hex, zipSHA256, err := md5AndSHA256File(zipPath)
	if err != nil {
		os.Remove(zipPath)
		return stagedItem{}, err
	}

	return stagedItem{
		group:      g,
		isZip:      true,
		zipTmpPath: zipPath,
		zipKey:     key,
		indexKey:   indexKey,
		zipRel:     zipRel,
		zipSize:    size,
		zipMD5hex:  md5hex,
		zipSHA256:  zipSHA256,
		zipEntries: entries,
	}, nil
}

// stageIndividualGroup copyAndHash's each file to a tmp path. Files that
// fail to copy are marked failed in the DB and omitted from the staged item.
//
// Tmp files use the stable name `ind-{fileID}` (no runID prefix) so a tmp
// left behind by a previous run's failed upload can be reused: tryReuseTmp
// checks size + mtime against the DB row and re-hashes locally. The engine
// refuses concurrent runs, so two runs never share a tmp. (#127)
func (e *Engine) stageIndividualGroup(ctx context.Context, runID int64, g Group) (stagedItem, error) {
	item := stagedItem{group: g, isZip: false}
	for i := 0; i < len(g.Files); i += e.opts.ChunkSize {
		j := i + e.opts.ChunkSize
		if j > len(g.Files) {
			j = len(g.Files)
		}
		for _, pf := range g.Files[i:j] {
			if err := ctx.Err(); err != nil {
				return stagedItem{}, err
			}
			if e.opts.StopRequested != nil && e.opts.StopRequested() {
				return stagedItem{}, ErrStopRequested
			}
			key := path.Join(e.opts.KeyPrefix, pf.RelPath)
			tmp := filepath.Join(e.opts.TmpDir, fmt.Sprintf("ind-%d", pf.ID))

			// Resume path: a usable cached tmp from a prior run's failed
			// upload skips the source read entirely. tryReuseTmp removes
			// stale / mismatched tmp itself so the fallback copy starts
			// clean.
			size, md5hex, sha256hex, reused, err := uploadHashes(tmp, pf)
			if err != nil {
				e.log(ctx, runID, db.LogWarn, fmt.Sprintf("inspect cached tmp %s: %v — falling back to copy", tmp, err))
			}
			if reused {
				e.log(ctx, runID, db.LogInfo, fmt.Sprintf("reusing cached tmp %s for %s", tmp, pf.RelPath))
				// No copy_progress emit on reuse — there was no source read.
			} else {
				size, md5hex, sha256hex, err = copyAndHash(ctx, e.opts.Source, pf.RelPath, tmp, e.copyWrap(runID, key, pf.Size))
				if err != nil {
					// Partial copy is unusable for next run's resume; remove
					// now so we don't poison a future tryReuseTmp with a
					// wrong-sized tmp.
					_ = os.Remove(tmp)
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return stagedItem{}, err
					}
					if mErr := e.opts.DB.MarkFailed(ctx, pf.ID); mErr != nil {
						e.log(ctx, runID, db.LogWarn, fmt.Sprintf("mark failed %s: %v (copy error: %v)", pf.RelPath, mErr, err))
					} else {
						e.log(ctx, runID, db.LogWarn, fmt.Sprintf("skip %s after copy error: %v", pf.RelPath, err))
					}
					continue
				}
				e.emitCopyProgress(runID, key, size, size)
			}
			item.individuals = append(item.individuals, stagedIndividual{
				pf:        pf,
				tmpPath:   tmp,
				key:       key,
				size:      size,
				md5hex:    md5hex,
				sha256hex: sha256hex,
			})
		}
	}
	return item, nil
}

// uploadStaged uploads a stagedItem to S3 and commits results to the DB.
func (e *Engine) uploadStaged(ctx context.Context, runID int64, item stagedItem) (int64, int64, error) {
	if item.isZip {
		return e.uploadStagedZip(ctx, runID, item)
	}
	return e.uploadStagedIndividuals(ctx, runID, item)
}

// uploadStagedZip uploads the staged zip and index sidecar to S3, then
// marks all zip members as uploaded in the DB.
func (e *Engine) uploadStagedZip(ctx context.Context, runID int64, item stagedItem) (int64, int64, error) {
	defer os.Remove(item.zipTmpPath)

	e.emit(Event{
		Type: EventUploadStart, RunID: runID, At: e.opts.Now(),
		Data: map[string]any{"key": item.zipKey, "size": item.zipSize, "files": len(item.group.Files)},
	})

	// Upload the STANDARD-tier index sidecar BEFORE the zip so a crash
	// between the two uploads is recoverable: reconcileFromS3 reads the
	// sidecar to mark files uploaded on the next run. (#121)
	// indexWrittenThisRun marks an index sidecar that *we* uploaded this
	// run. Cleanup on a subsequent zip-upload failure must only delete the
	// sidecar we wrote — a pre-existing matched sidecar from an earlier
	// failed run belongs to that prior content and shouldn't be removed
	// from under it. (#240)
	indexWrittenThisRun := false
	if e.opts.EnableZipIndex {
		indexBody := strings.Join(item.zipEntries, "\n") + "\n"
		indexSum := sha256.Sum256([]byte(indexBody))
		indexSHA256 := hex.EncodeToString(indexSum[:])
		if e.skipIfMatches(ctx, item.indexKey, indexSHA256) {
			e.log(ctx, runID, db.LogInfo, fmt.Sprintf("skip zip index %s: SHA256 matches existing object", item.indexKey))
		} else {
			if _, err := e.opts.Storage.PutStandard(ctx, item.indexKey, strings.NewReader(indexBody), int64(len(indexBody))); err != nil {
				e.emit(Event{
					Type: EventUploadFailed, RunID: runID, At: e.opts.Now(),
					Data: map[string]any{"key": item.indexKey, "error": err.Error()},
				})
				return 0, 0, fmt.Errorf("upload zip index %s: %w", item.indexKey, err)
			}
			indexWrittenThisRun = true
			e.log(ctx, runID, db.LogInfo, fmt.Sprintf("uploaded zip index %s (%d entries)", item.indexKey, len(item.zipEntries)))
		}
	}

	now := e.opts.Now()
	ids := make([]int64, 0, len(item.group.Files))
	for _, f := range item.group.Files {
		ids = append(ids, f.ID)
	}

	// Dedup: skip PutIfAbsent when S3 already holds the identical zip. (#133)
	if e.skipIfMatches(ctx, item.zipKey, item.zipSHA256) {
		e.log(ctx, runID, db.LogInfo, fmt.Sprintf("skip zip upload %s: SHA256 matches existing object", item.zipKey))
		if err := e.opts.DB.MarkZipUploadedBatch(ctx, ids, item.zipRel, item.zipMD5hex, item.zipKey, now); err != nil {
			return 0, 0, fmt.Errorf("mark uploaded (zip %s): %w", item.zipRel, err)
		}
		e.emit(Event{
			Type: EventUploadComplete, RunID: runID, At: now,
			Data: map[string]any{"key": item.zipKey, "size": item.zipSize, "checksum_sha256": item.zipSHA256, "files": len(item.group.Files), "skipped": true},
		})
		return int64(len(item.group.Files)), item.zipSize, nil
	}

	var (
		res storage.PutResult
		err error
	)
	if _, _, ok := e.shouldUseResumable(item.zipSize); ok {
		// Resumable path: persist UploadId across runs. IfAbsent
		// preserves the existing zip-collision retry semantics — a
		// HEAD probe up front returns ErrAlreadyExists for the
		// engine to advance the counter slot. (#162)
		res, err = e.putResumable(ctx, runID, item.zipKey, item.zipTmpPath, item.zipSize, item.zipSHA256, resumePutOpts{
			ZipKey:   item.zipRel,
			IfAbsent: true,
		})
	} else {
		// Wrap in a closure so defer f.Close() fires even if a panic
		// lands in uploadProgressCtx, the SDK call, or an Emit sink —
		// the previous manual Close after the Put leaked the fd in that
		// case. (#259)
		// PutIfAbsent so a retry under the same key can't silently overwrite a
		// prior DEEP_ARCHIVE object whose content may differ; the caller catches
		// ErrAlreadyExists and advances the counter slot. (#116)
		res, err = func() (storage.PutResult, error) {
			f, openErr := os.Open(item.zipTmpPath)
			if openErr != nil {
				return storage.PutResult{}, openErr
			}
			defer f.Close()
			return e.opts.Storage.PutIfAbsent(e.uploadProgressCtx(ctx, runID, item.zipKey, item.zipSize), item.zipKey, f, item.zipSize)
		}()
	}
	if err != nil {
		// Leave tmp in place — next run can reuse it instead of re-reading
		// the source. A genuinely-broken tmp gets cleaned up on the next
		// run's tryReuseTmp size/mtime check or by sweepOrphanTmps if the
		// row leaves the pending list. (#127)
		e.emit(Event{
			Type: EventUploadFailed, RunID: runID, At: e.opts.Now(),
			Data: map[string]any{"key": item.zipKey, "error": err.Error()},
		})
		if indexWrittenThisRun {
			if delErr := e.opts.Storage.Delete(ctx, item.indexKey); delErr != nil {
				e.log(ctx, runID, db.LogWarn, fmt.Sprintf("cleanup orphan index %s: %v", item.indexKey, delErr))
			}
		}
		return 0, 0, fmt.Errorf("upload %s: %w", item.zipKey, err)
	}

	if err := e.opts.DB.MarkZipUploadedBatch(ctx, ids, item.zipRel, item.zipMD5hex, item.zipKey, now); err != nil {
		return 0, 0, fmt.Errorf("mark uploaded (zip %s): %w", item.zipRel, err)
	}

	e.log(ctx, runID, db.LogInfo, fmt.Sprintf("uploaded %s (%d bytes, etag=%s)", item.zipKey, item.zipSize, res.ETag))
	e.emit(Event{
		Type: EventUploadComplete, RunID: runID, At: now,
		Data: map[string]any{"key": item.zipKey, "size": item.zipSize, "etag": res.ETag, "checksum_sha256": res.ChecksumSHA256, "files": len(item.group.Files)},
	})
	return int64(len(item.group.Files)), item.zipSize, nil
}

// uploadStagedIndividuals uploads each pre-staged individual file to S3.
// On cancel / stop / per-file failure, staged tmps are deliberately left in
// place so the next run's stageIndividualGroup can reuse them via
// tryReuseTmp; sweepOrphanTmps keeps the dir bounded across runs. (#127)
func (e *Engine) uploadStagedIndividuals(ctx context.Context, runID int64, item stagedItem) (int64, int64, error) {
	var uploaded, bytes int64
	for _, ind := range item.individuals {
		if err := ctx.Err(); err != nil {
			return uploaded, bytes, err
		}
		if e.opts.StopRequested != nil && e.opts.StopRequested() {
			return uploaded, bytes, ErrStopRequested
		}
		n, err := e.uploadOneIndividual(ctx, runID, ind)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return uploaded, bytes, err
			}
			e.log(ctx, runID, db.LogWarn, fmt.Sprintf("skip %s after upload error: %v", ind.pf.RelPath, err))
			continue
		}
		uploaded++
		bytes += n
	}
	return uploaded, bytes, nil
}

// uploadOneIndividual streams one pre-staged tmp file to S3. Tmp is removed
// only on success or dedup-skip; on upload failure the tmp is preserved so
// the next run can reuse it via tryReuseTmp instead of re-reading the
// source. (#127)
func (e *Engine) uploadOneIndividual(ctx context.Context, runID int64, ind stagedIndividual) (int64, error) {
	now := e.opts.Now()

	// Dedup: skip upload when S3 already holds the identical object. (#133)
	if e.skipIfMatches(ctx, ind.key, ind.sha256hex) {
		e.log(ctx, runID, db.LogInfo, fmt.Sprintf("skip upload %s: SHA256 matches existing object", ind.key))
		_ = os.Remove(ind.tmpPath) // content is in S3 — don't leak
		e.buf.markUploaded(ctx, ind.pf.ID, ind.md5hex, ind.key, now)
		e.emit(Event{
			Type: EventUploadComplete, RunID: runID, At: now,
			Data: map[string]any{"key": ind.key, "size": ind.size, "checksum_sha256": ind.sha256hex, "skipped": true},
		})
		return ind.size, nil
	}

	e.emit(Event{
		Type: EventUploadStart, RunID: runID, At: e.opts.Now(),
		Data: map[string]any{"key": ind.key, "size": ind.size},
	})

	var (
		res storage.PutResult
		err error
	)
	if _, _, ok := e.shouldUseResumable(ind.size); ok {
		// Resumable path: persist UploadId across runs so a mid-upload
		// crash doesn't waste the parts S3 already accepted. (#162)
		res, err = e.putResumable(ctx, runID, ind.key, ind.tmpPath, ind.size, ind.sha256hex, resumePutOpts{
			FileID: ind.pf.ID,
		})
	} else {
		// defer Close via closure so a panic mid-Put doesn't leak the fd. (#259)
		res, err = func() (storage.PutResult, error) {
			f, openErr := os.Open(ind.tmpPath)
			if openErr != nil {
				return storage.PutResult{}, openErr
			}
			defer f.Close()
			return e.opts.Storage.Put(e.uploadProgressCtx(ctx, runID, ind.key, ind.size), ind.key, f, ind.size)
		}()
	}
	if err != nil {
		// Leave tmp in place — next run's stageIndividualGroup picks it up
		// via tryReuseTmp instead of re-reading the source. (#127)
		e.emit(Event{
			Type: EventUploadFailed, RunID: runID, At: e.opts.Now(),
			Data: map[string]any{"key": ind.key, "error": err.Error()},
		})
		if mErr := e.opts.DB.MarkFailed(ctx, ind.pf.ID); mErr != nil {
			return 0, fmt.Errorf("upload %s: %w (and mark failed: %v)", ind.key, err, mErr)
		}
		return 0, fmt.Errorf("upload %s: %w", ind.key, err)
	}

	// Successful upload — free tmp now. We deliberately don't defer this
	// at function entry so the failure path above can keep tmp for resume.
	_ = os.Remove(ind.tmpPath)
	e.buf.markUploaded(ctx, ind.pf.ID, ind.md5hex, ind.key, now)
	e.emit(Event{
		Type: EventUploadComplete, RunID: runID, At: now,
		Data: map[string]any{"key": ind.key, "size": ind.size, "etag": res.ETag, "checksum_sha256": res.ChecksumSHA256},
	})
	return ind.size, nil
}

// uploadHashes returns (size, md5, sha256, reused, err). When tryReuseTmp
// reports a usable cached tmp, the returned hashes come from re-hashing
// it locally (one disk pass) and reused=true. Otherwise the caller must
// run copyAndHash and overwrite tmp.
func uploadHashes(tmp string, pf PendingFile) (size int64, md5hex, sha256hex string, reused bool, err error) {
	md5hex, sha256hex, ok, err := tryReuseTmp(tmp, pf)
	if err != nil || !ok {
		return 0, "", "", false, err
	}
	return pf.Size, md5hex, sha256hex, true, nil
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
	var finalEmitted atomic.Bool
	return storage.WithUploadProgress(ctx, func(n int64) {
		total := sent.Add(n)
		nowNs := e.opts.Now().UnixNano()
		isFinal := size > 0 && total >= size
		// The final 100% sample is must-emit-once: a CAS race against
		// another concurrent multipart callback used to drop it silently,
		// leaving the UI stuck at the last throttled sample. Use a
		// dedicated bool with CAS so exactly one caller wins. (#257)
		if isFinal {
			if finalEmitted.CompareAndSwap(false, true) {
				lastEmitNs.Store(nowNs)
				e.emitUploadProgress(runID, key, total, size)
			}
			return
		}
		for {
			last := lastEmitNs.Load()
			if last != 0 && nowNs-last < int64(defaultProgressInterval) {
				return
			}
			if lastEmitNs.CompareAndSwap(last, nowNs) {
				e.emitUploadProgress(runID, key, total, size)
				return
			}
			// CAS lost: retry under the new last so a coarse-clock tie
			// doesn't drop the emit and a real progress beat still fires.
		}
	})
}

func (e *Engine) log(ctx context.Context, runID int64, level, msg string) {
	if e.buf != nil {
		e.buf.appendLog(ctx, runID, level, msg, e.opts.Now())
		return
	}
	_ = e.opts.DB.AppendLog(ctx, runID, level, msg, e.opts.Now())
}

// ctxReader wraps an io.Reader and returns ctx.Err() from Read after
// cancellation. Without this, an io.Copy off a *os.File / SMB session
// keeps reading even when the run ctx is cancelled — a multi-GB source
// file would block /api/cancel for the full read time. The granularity
// is one io.Copy buffer (32 KiB by default), which is fast enough that
// cancel is observed within milliseconds of fire on any real disk.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// copyAndHash copies a source entry to disk, computing both md5 and
// sha256 on the way. md5 is what we persist in the DB for legacy
// reasons; sha256 is used transiently by the dedup check that skips
// the upload when S3 already holds an object with the same SHA256.
// (#133) wrap, if non-nil, wraps the source reader before bytes flow
// into the writer — the engine uses this to inject a progressReader
// that emits live copy_progress events for slow / large source reads.
func copyAndHash(ctx context.Context, src source.Source, rel, tmp string, wrap func(io.Reader) io.Reader) (n int64, md5hex, sha256hex string, retErr error) {
	rc, err := src.Open(ctx, rel)
	if err != nil {
		return 0, "", "", err
	}
	defer rc.Close()

	out, err := os.Create(tmp)
	if err != nil {
		return 0, "", "", err
	}
	// Capture Close errors: on filesystems that defer disk-write reporting
	// (NFS, some SMB mounts, ENOSPC after delayed allocation), write errors
	// only surface on Close. A silent close lets corrupt tmp files through
	// to the upload path. (#145)
	defer func() {
		if cerr := out.Close(); cerr != nil && retErr == nil {
			retErr = cerr
		}
	}()

	var reader io.Reader = rc
	if wrap != nil {
		reader = wrap(rc)
	}
	// Make the copy ctx-aware so /api/cancel exits the read loop within
	// one buffer instead of waiting for the source file to drain.
	reader = &ctxReader{ctx: ctx, r: reader}

	hMD5 := md5.New()
	hSHA := sha256.New()
	n, err = io.Copy(io.MultiWriter(out, hMD5, hSHA), reader)
	if err != nil {
		return n, "", "", err
	}
	if err := out.Sync(); err != nil {
		return n, "", "", err
	}
	return n, hex.EncodeToString(hMD5.Sum(nil)), hex.EncodeToString(hSHA.Sum(nil)), nil
}

// md5AndSHA256File returns hex MD5 and hex SHA256 in a single pass
// through the file, so a multi-GiB staged zip isn't read off disk
// twice on the hot copy→upload path. (#254)
func md5AndSHA256File(p string) (md5hex string, sha256hex string, err error) {
	f, ferr := os.Open(p)
	if ferr != nil {
		return "", "", ferr
	}
	defer f.Close()
	hMD5 := md5.New()
	hSHA := sha256.New()
	if _, err := io.Copy(io.MultiWriter(hMD5, hSHA), f); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(hMD5.Sum(nil)), hex.EncodeToString(hSHA.Sum(nil)), nil
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

// tryReuseTmp inspects an existing tmp file and returns (md5, sha256,
// true, nil) if it can stand in for a fresh source→tmp copy: the size
// matches the DB row and the tmp's mtime is >= the DB-recorded source
// mtime (so the source can't have been modified after the cached copy
// was written). Re-hashing on reuse catches bit-rot or partial writes
// from a previous crash. A stale or unreadable tmp is removed and
// (false, nil) is returned so the caller falls through to a regular
// copyAndHash. (#127)
func tryReuseTmp(tmp string, pf PendingFile) (md5hex, sha256hex string, ok bool, err error) {
	info, statErr := os.Stat(tmp)
	if errors.Is(statErr, os.ErrNotExist) {
		return "", "", false, nil
	}
	if statErr != nil {
		return "", "", false, statErr
	}
	if info.Size() != pf.Size {
		_ = os.Remove(tmp)
		return "", "", false, nil
	}
	if !pf.MTime.IsZero() && info.ModTime().Before(pf.MTime) {
		_ = os.Remove(tmp)
		return "", "", false, nil
	}
	f, openErr := os.Open(tmp)
	if openErr != nil {
		return "", "", false, openErr
	}
	defer f.Close()
	hMD5 := md5.New()
	hSHA := sha256.New()
	if _, err := io.Copy(io.MultiWriter(hMD5, hSHA), f); err != nil {
		return "", "", false, err
	}
	return hex.EncodeToString(hMD5.Sum(nil)), hex.EncodeToString(hSHA.Sum(nil)), true, nil
}

// sweepOrphanTmps walks tmpDir once and deletes any `ind-{N}` file
// whose ID is not in keep. This bounds tmp's size to "files actively
// pending" (the issue title's literal requirement); without it, every
// failed upload would leak a file that the next runs never touch
// because reconcile or another path already advanced its DB row past
// pending. Best-effort: per-entry errors are logged and skipped. (#127)
func (e *Engine) sweepOrphanTmps(ctx context.Context, runID int64, tmpDir string, keep map[int64]struct{}) {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		e.log(ctx, runID, db.LogWarn, fmt.Sprintf("orphan tmp sweep: read %s: %v", tmpDir, err))
		return
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		const prefix = "ind-"
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		idStr := strings.TrimPrefix(name, prefix)
		id, parseErr := strconv.ParseInt(idStr, 10, 64)
		if parseErr != nil {
			continue
		}
		if _, ok := keep[id]; ok {
			continue
		}
		if rmErr := os.Remove(filepath.Join(tmpDir, name)); rmErr != nil {
			e.log(ctx, runID, db.LogWarn, fmt.Sprintf("orphan tmp sweep: remove %s: %v", name, rmErr))
		}
	}
}
