package source

import (
	"context"
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

// Scan walks src, upserts every entry into the DB, and flips previously
// uploaded rows whose last_seen_at predates the scan start to 'missing'.
//
// When paths is non-empty, only files whose RelPath matches or is under one
// of the given paths are processed (partial rescan). Missing-detection is
// skipped for partial scans because the walker only visited a subset.
func Scan(ctx context.Context, src Source, d *db.DB, paths []string, log Logger) (ScanStats, error) {
	var stats ScanStats
	scanStart := time.Now().UTC()

	err := src.Walk(ctx, func(e Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(paths) > 0 && !matchesAnyPath(e.RelPath, paths) {
			return nil
		}
		stats.Seen++
		r, err := d.UpsertFile(ctx, e.RelPath, e.Size, e.ModTime, scanStart)
		if err != nil {
			return err
		}
		switch {
		case r.Created:
			stats.New++
			if log != nil {
				log("new: " + e.RelPath)
			}
		case r.Changed:
			stats.Changed++
			if log != nil {
				log("changed: " + e.RelPath)
			}
		default:
			stats.Unchanged++
		}
		return nil
	})
	if err != nil {
		return stats, err
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
			log("marked missing: " + itoa(missing))
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

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
