package source

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
)

// scanDB is the narrow DB surface Scan needs. Keeping it small lets tests
// inject a spy without having to stand up a full sqlite-backed DB.
type scanDB interface {
	UpsertFileBatch(ctx context.Context, entries []db.BatchEntry, seenAt time.Time) ([]db.UpsertResult, error)
	MarkMissing(ctx context.Context, scanStart time.Time) (int64, error)
}

// ScanStats summarises the outcome of a Scan call.
type ScanStats struct {
	Seen      int64 // files observed during walk
	Bytes     int64 // total bytes observed during walk
	New       int64 // rows inserted into DB
	Changed   int64 // existing rows whose size/mtime differed
	Unchanged int64 // existing rows with no size/mtime change
	Missing   int64 // rows reclassified to missing by a full local scan
}

// ScanBatchResult reports whether the walk stopped early because the byte
// budget was hit, plus the directory subtrees that finished in this batch.
type ScanBatchResult struct {
	CompletedFolders []string
	Paused           bool
	PausePath        string
}

// Logger is a minimal sink for scan-level messages. nil is fine.
type Logger func(msg string)

// ScanProgress carries the cumulative running totals reported during the
// walk. The fields mirror the corresponding ScanStats values at the
// moment the latest batch was committed.
type ScanProgress struct {
	Seen    int64
	Bytes   int64
	New     int64
	Changed int64
}

// ProgressFn is invoked from the flusher goroutine after each successful
// batch upsert. nil is fine.
type ProgressFn func(ScanProgress)

const (
	flushInterval = 3 * time.Second
	// walkProgressInterval throttles scan_progress emissions driven by
	// the walker so a fast Walk on a million-file tree doesn't fan
	// out a million events to the SSE bus. The walker reports "files
	// seen" — the user-facing intuition — independently of whether
	// UpsertFileBatch has committed them yet, so the indicator
	// updates within ~250 ms of every batch of ~500 walked entries.
	walkProgressInterval = 250 * time.Millisecond
	walkProgressEvery    = 500
)

// Scan walks src, accumulates discovered files in RAM, and flushes them to the
// DB in batches every few seconds or whenever buf reaches batchSize. When
// batchBytes is positive, the walker yields after it finishes the current
// directory subtree and the batch's total file size crosses that soft limit.
//
// When paths is non-empty, only files whose RelPath matches or is under one
// of the given paths are processed (partial rescan). Missing-detection is
// skipped for partial scans because the walker only visited a subset.
func Scan(
	ctx context.Context,
	src Source,
	d scanDB,
	paths []string,
	skipFolders map[string]struct{},
	log Logger,
	onProgress ProgressFn,
	batchSize int,
	batchBytes int64,
) (ScanStats, ScanBatchResult, error) {
	var stats ScanStats
	scanStart := time.Now().UTC()
	if batchSize <= 0 {
		batchSize = 1
	}

	var (
		mu  sync.Mutex
		buf []db.BatchEntry
	)
	var flushMu sync.Mutex

	// walkSeen / atomicNew / atomicChanged are written by walker and
	// flusher concurrently and read by emitWalkProgress under no lock.
	// stats (the function-return aggregate) is hydrated from these at
	// return time.
	var walkSeen, walkBytes, atomicNew, atomicChanged, atomicUnchanged atomic.Int64
	var batchBytesSeen int64
	var (
		lastEmitMu  sync.Mutex
		lastEmitAt  time.Time
		lastEmitVal int64
	)
	emitWalkProgress := func(force bool) {
		if onProgress == nil {
			return
		}
		seen := walkSeen.Load()
		lastEmitMu.Lock()
		now := time.Now()
		if !force {
			if seen-lastEmitVal < walkProgressEvery && now.Sub(lastEmitAt) < walkProgressInterval {
				lastEmitMu.Unlock()
				return
			}
		}
		lastEmitAt = now
		lastEmitVal = seen
		lastEmitMu.Unlock()
		onProgress(ScanProgress{
			Seen:    seen,
			Bytes:   walkBytes.Load(),
			New:     atomicNew.Load(),
			Changed: atomicChanged.Load(),
		})
	}

	// flush drains the buffer and upserts into DB. flushMu serializes the
	// timer and batch-triggered callers so only one upsert runs at a time.
	flush := func() error {
		flushMu.Lock()
		defer flushMu.Unlock()

		mu.Lock()
		if len(buf) == 0 {
			mu.Unlock()
			return nil
		}
		entries := make([]db.BatchEntry, len(buf))
		copy(entries, buf)
		buf = buf[:0]
		mu.Unlock()

		results, err := d.UpsertFileBatch(ctx, entries, scanStart)
		if err != nil {
			return err
		}
		for i, r := range results {
			switch {
			case r.Created:
				atomicNew.Add(1)
				if log != nil {
					log("new: " + entries[i].Path)
				}
			case r.Changed:
				atomicChanged.Add(1)
				if log != nil {
					log("changed: " + entries[i].Path)
				}
			default:
				atomicUnchanged.Add(1)
			}
		}
		emitWalkProgress(true)
		return nil
	}

	// scanCtx is cancelled by the flusher when a tick-flush errors so the
	// walker stops appending immediately — otherwise the buffer would grow
	// unbounded and silently discard every entry walked after the failure.
	scanCtx, cancelScan := context.WithCancelCause(ctx)
	defer cancelScan(nil)

	done := make(chan struct{})
	flushErrCh := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				flushErrCh <- fmt.Errorf("flush panic: %v", r)
			}
		}()
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := flush(); err != nil {
					cancelScan(err)
					flushErrCh <- err
					return
				}
			case <-done:
				flushErrCh <- flush()
				return
			}
		}
	}()

	type dirFrame struct {
		path string
	}

	var (
		completedFolders []string
		dirStack         []dirFrame
		stopAfter        string
		paused           bool
	)
	uniqueAppend := func(dst []string, v string) []string {
		if v == "" {
			return dst
		}
		if len(dst) == 0 || dst[len(dst)-1] != v {
			dst = append(dst, v)
		}
		return dst
	}
	isUnder := func(rel, prefix string) bool {
		if prefix == "" {
			return true
		}
		if rel == prefix {
			return true
		}
		if len(rel) > len(prefix) && strings.HasPrefix(rel, prefix) && rel[len(prefix)] == '/' {
			return true
		}
		return false
	}
	matchesPartial := func(rel string, targets []string) bool {
		for _, t := range targets {
			if t == "" || t == "/" {
				return true
			}
			if rel == t {
				return true
			}
			if len(rel) > len(t) && strings.HasPrefix(rel, t) && rel[len(t)] == '/' {
				return true
			}
			if len(t) > len(rel) && strings.HasPrefix(t, rel) && t[len(rel)] == '/' {
				return true
			}
		}
		return false
	}
	currentDir := func() string {
		if len(dirStack) == 0 {
			return ""
		}
		return dirStack[len(dirStack)-1].path
	}
	popFinished := func(next string) {
		for len(dirStack) > 0 {
			top := dirStack[len(dirStack)-1].path
			if next != "" && isUnder(next, top) {
				break
			}
			dirStack = dirStack[:len(dirStack)-1]
			completedFolders = uniqueAppend(completedFolders, top)
		}
	}

	const stopWalkErrText = "source: scan batch paused"
	stopWalkErr := errors.New(stopWalkErrText)

	walkErr := src.Walk(scanCtx, func(e Entry) error {
		if err := scanCtx.Err(); err != nil {
			return err
		}
		if !e.IsDir && len(paths) > 0 && !matchesPartial(e.RelPath, paths) {
			return nil
		}
		if e.IsDir && len(paths) > 0 && !matchesPartial(e.RelPath, paths) {
			return ErrSkipDir
		}

		popFinished(e.RelPath)
		if paused && stopAfter != "" && !isUnder(e.RelPath, stopAfter) {
			return stopWalkErr
		}

		if e.IsDir {
			if _, ok := skipFolders[e.RelPath]; ok {
				return ErrSkipDir
			}
			dirStack = append(dirStack, dirFrame{path: e.RelPath})
			return nil
		}

		mu.Lock()
		buf = append(buf, db.BatchEntry{Path: e.RelPath, Size: e.Size, ModTime: e.ModTime})
		bufLen := len(buf)
		mu.Unlock()
		walkSeen.Add(1)
		walkBytes.Add(e.Size)
		batchBytesSeen += e.Size
		emitWalkProgress(false)
		if batchBytes > 0 && !paused {
			if batchBytesSeen >= batchBytes {
				paused = true
				stopAfter = currentDir()
				if stopAfter == "" {
					stopAfter = e.RelPath
				}
			}
		}
		if bufLen >= batchSize {
			if err := flush(); err != nil {
				cancelScan(err)
				return err
			}
		}
		return nil
	})

	close(done)
	flushErr := <-flushErrCh

	// Close out any directories that were fully traversed before the walk
	// ended or before the pause sentinel fired.
	popFinished("")

	// Hydrate the return aggregate from the atomic counters now that
	// both walker and flusher are done writing them.
	stats.New = atomicNew.Load()
	stats.Changed = atomicChanged.Load()
	stats.Unchanged = atomicUnchanged.Load()
	stats.Seen = stats.New + stats.Changed + stats.Unchanged
	stats.Bytes = walkBytes.Load()

	if flushErr != nil {
		return stats, ScanBatchResult{}, flushErr
	}
	if walkErr != nil && !errors.Is(walkErr, stopWalkErr) {
		if cause := context.Cause(scanCtx); cause != nil && cause != ctx.Err() && cause != context.Canceled {
			return stats, ScanBatchResult{}, cause
		}
		return stats, ScanBatchResult{}, walkErr
	}

	// Only classify disappearances on a full scan with no skipped folders;
	// batch-resume scans intentionally skip already-completed subtrees, so
	// treating those unseen rows as missing would fight the resumable skip-set.
	if len(paths) == 0 && !paused && len(skipFolders) == 0 {
		missing, err := d.MarkMissing(ctx, scanStart)
		if err != nil {
			return stats, ScanBatchResult{}, err
		}
		stats.Missing = missing
		if missing > 0 && log != nil {
			log("marked missing: " + strconv.FormatInt(missing, 10))
		}
	}

	return stats, ScanBatchResult{
		CompletedFolders: completedFolders,
		Paused:           paused && errors.Is(walkErr, stopWalkErr),
		PausePath:        stopAfter,
	}, nil
}
