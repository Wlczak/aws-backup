package source

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
)

// ScanStats summarises the outcome of a Scan call.
type ScanStats struct {
	Seen      int64 // files observed during walk
	New       int64 // rows inserted into DB
	Changed   int64 // existing rows whose size/mtime differed
	Unchanged int64 // existing rows with no size/mtime change
	Missing   int64 // previously uploaded rows no longer present
}

// Logger is a minimal sink for scan-level messages. nil is fine.
type Logger func(msg string)

const flushInterval = 3 * time.Second

// Scan walks src, accumulates discovered files in RAM, and flushes them to the
// DB in batches every few seconds. This avoids hammering SQLite with one
// transaction per file during large scans.
//
// When paths is non-empty, only files whose RelPath matches or is under one
// of the given paths are processed (partial rescan). Missing-detection is
// skipped for partial scans because the walker only visited a subset.
func Scan(ctx context.Context, src Source, d *db.DB, paths []string, log Logger) (ScanStats, error) {
	var stats ScanStats
	scanStart := time.Now().UTC()

	var (
		mu  sync.Mutex
		buf []db.BatchEntry
	)

	// flush drains the buffer and upserts into DB. Called only from the
	// flusher goroutine so stats are updated without a lock.
	flush := func() error {
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
			stats.Seen++
			switch {
			case r.Created:
				stats.New++
				if log != nil {
					log("new: " + entries[i].Path)
				}
			case r.Changed:
				stats.Changed++
				if log != nil {
					log("changed: " + entries[i].Path)
				}
			default:
				stats.Unchanged++
			}
		}
		return nil
	}

	done := make(chan struct{})
	flushErrCh := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := flush(); err != nil {
					flushErrCh <- err
					return
				}
			case <-done:
				flushErrCh <- flush()
				return
			}
		}
	}()

	walkErr := src.Walk(ctx, func(e Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(paths) > 0 && !matchesAnyPath(e.RelPath, paths) {
			return nil
		}
		mu.Lock()
		buf = append(buf, db.BatchEntry{Path: e.RelPath, Size: e.Size, ModTime: e.ModTime})
		mu.Unlock()
		return nil
	})

	close(done)
	flushErr := <-flushErrCh
	if walkErr != nil {
		return stats, walkErr
	}
	if flushErr != nil {
		return stats, flushErr
	}

	// Only detect missing files on a full scan; partial scans only walked a
	// subset so any unvisited files should not be marked as missing.
	if len(paths) == 0 {
		missing, err := d.MarkMissing(ctx, scanStart)
		if err != nil {
			return stats, err
		}
		stats.Missing = missing
		if missing > 0 && log != nil {
			log("marked missing: " + strconv.FormatInt(missing, 10))
		}
	}
	return stats, nil
}

// matchesAnyPath reports whether relPath equals or is under any of the
// target paths (path-component boundary aware).
func matchesAnyPath(relPath string, targets []string) bool {
	for _, t := range targets {
		if t == "" || t == "/" {
			return true
		}
		if relPath == t {
			return true
		}
		// check prefix at component boundary: target "foo/bar" matches "foo/bar/baz"
		if len(relPath) > len(t) && relPath[len(t)] == '/' && relPath[:len(t)] == t {
			return true
		}
	}
	return false
}

