package engine

import (
	"io"
	"time"
)

// defaultProgressInterval is how often a single upload may emit an
// EventUploadProgress event. Throttling here keeps the SSE bus from
// being flooded by per-Read callbacks (transfermanager reads in small
// part-sized chunks).
const defaultProgressInterval = 250 * time.Millisecond

// progressReader wraps an io.Reader and invokes onProgress as bytes
// flow through Read. The callback is throttled to at most one call per
// interval, plus a final call once the underlying reader signals io.EOF
// so the final 100% sample is never lost to throttling.
//
// Not safe for concurrent Read calls — but the AWS SDK doesn't read
// concurrently from a non-seekable Reader, and we wrap the body as a
// plain io.Reader (hiding *os.File's Seeker/ReaderAt) precisely so that
// every byte uploaded passes through this Read once, in order.
type progressReader struct {
	r          io.Reader
	total      int64
	read       int64
	lastEmit   time.Time
	interval   time.Duration
	onProgress func(read, total int64)
	now        func() time.Time
}

func newProgressReader(r io.Reader, total int64, interval time.Duration, onProgress func(read, total int64)) *progressReader {
	if interval <= 0 {
		interval = defaultProgressInterval
	}
	return &progressReader{
		r:          r,
		total:      total,
		interval:   interval,
		onProgress: onProgress,
		now:        time.Now,
	}
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		pr.read += int64(n)
	}
	if pr.onProgress != nil {
		now := pr.now()
		emit := false
		if err == io.EOF {
			emit = true
		} else if n > 0 && (pr.lastEmit.IsZero() || now.Sub(pr.lastEmit) >= pr.interval) {
			emit = true
		}
		if emit {
			pr.lastEmit = now
			pr.onProgress(pr.read, pr.total)
		}
	}
	return n, err
}

// counterReader is a minimal io.Reader wrapper that fires onRead with
// the byte count of every Read. No throttling — the caller is expected
// to throttle its emit logic externally. Used by the zip copy phase
// where bytes from many sequential per-file readers feed one shared
// running total against a group-level size; per-reader throttling
// would reset on every file boundary and cause emit storms.
type counterReader struct {
	r      io.Reader
	onRead func(n int)
}

func (cr *counterReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 && cr.onRead != nil {
		cr.onRead(n)
	}
	return n, err
}
