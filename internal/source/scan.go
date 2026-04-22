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
// The timestamp captured at the start is reused as last_seen_at for every
// row so "missing" is a clean "not seen in this scan" check.
func Scan(ctx context.Context, src Source, d *db.DB, log Logger) (ScanStats, error) {
	var stats ScanStats
	scanStart := time.Now().UTC()

	err := src.Walk(ctx, func(e Entry) error {
		if err := ctx.Err(); err != nil {
			return err
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

	missing, err := d.MarkMissing(ctx, scanStart)
	if err != nil {
		return stats, err
	}
	stats.Missing = missing
	if missing > 0 && log != nil {
		log("marked missing: " + itoa(missing))
	}
	return stats, nil
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
