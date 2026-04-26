package engine

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
)

// Default flush tuning. Tuned for ~300k-file runs: at 500 entries/flush
// the engine produces ~600 commits per run instead of 300k, and the 3s
// ticker bounds staleness when the rate is low (end of a run, idle API).
const (
	bufferFlushInterval = 3 * time.Second
	bufferFlushSize     = 500
)

// writeBuffer coalesces per-file DB writes the engine would otherwise do
// one-at-a-time. It owns a background flusher goroutine for the lifetime
// of a run and drains on Close so nothing is lost on a clean shutdown.
//
// Crash semantics: a process kill between an S3 PUT and the next flush
// leaves the file marked 'pending' in the DB. The next run re-uploads it
// — same behavior as an interrupted run today, so no data loss.
type writeBuffer struct {
	d *db.DB

	mu      sync.Mutex
	uploads []db.UploadedRow
	logs    []db.LogEntry

	flushInterval time.Duration
	flushSize     int

	stop chan struct{}
	done chan struct{}
}

func newWriteBuffer(d *db.DB) *writeBuffer {
	return &writeBuffer{
		d:             d,
		flushInterval: bufferFlushInterval,
		flushSize:     bufferFlushSize,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
}

// start launches the background flusher. The caller must eventually call
// close(); passing a cancelled ctx to close() still performs a final
// flush on a detached context so nothing queued is lost.
func (b *writeBuffer) start(ctx context.Context) {
	go b.loop(ctx)
}

func (b *writeBuffer) loop(ctx context.Context) {
	defer close(b.done)
	t := time.NewTicker(b.flushInterval)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if err := b.flush(ctx); err != nil {
				slog.Warn("write buffer flush failed (will retry on next tick)", "err", err)
			}
		}
	}
}

// markUploaded queues an individual upload result. Flushes inline when
// the buffer reaches flushSize so bursty runs don't balloon memory.
func (b *writeBuffer) markUploaded(ctx context.Context, id int64, md5, s3Key string, at time.Time) {
	b.mu.Lock()
	b.uploads = append(b.uploads, db.UploadedRow{ID: id, MD5: md5, S3Key: s3Key, UploadedAt: at})
	full := len(b.uploads) >= b.flushSize
	b.mu.Unlock()
	if full {
		if err := b.flush(ctx); err != nil {
			slog.Warn("write buffer flush failed (will retry on next tick)", "err", err)
		}
	}
}

// appendLog queues a run log entry with the same size-threshold flush.
func (b *writeBuffer) appendLog(ctx context.Context, runID int64, level, msg string, at time.Time) {
	b.mu.Lock()
	b.logs = append(b.logs, db.LogEntry{RunID: runID, At: at, Level: level, Message: msg})
	full := len(b.logs) >= b.flushSize
	b.mu.Unlock()
	if full {
		if err := b.flush(ctx); err != nil {
			slog.Warn("write buffer flush failed (will retry on next tick)", "err", err)
		}
	}
}

// flush drains the current uploads+logs buffers to the DB. Swaps slices
// under the lock so the flush itself happens outside the critical section
// — cheap enqueues don't block on a slow commit. On error the unflushed
// entries are re-prepended into the queue so the next flush retries them
// instead of silently dropping rows that were already uploaded to S3.
func (b *writeBuffer) flush(ctx context.Context) error {
	b.mu.Lock()
	uploads := b.uploads
	logs := b.logs
	b.uploads = nil
	b.logs = nil
	b.mu.Unlock()

	if err := b.d.MarkUploadedMany(ctx, uploads); err != nil {
		b.requeue(uploads, logs)
		return err
	}
	if err := b.d.AppendLogMany(ctx, logs); err != nil {
		// Uploads already committed; only logs need requeueing.
		b.requeue(nil, logs)
		return err
	}
	return nil
}

// requeue puts un-committed entries back at the head of the buffers so
// the next flush attempt retries them. Preserves ordering relative to
// any entries enqueued in the meantime.
func (b *writeBuffer) requeue(uploads []db.UploadedRow, logs []db.LogEntry) {
	if len(uploads) == 0 && len(logs) == 0 {
		return
	}
	b.mu.Lock()
	if len(uploads) > 0 {
		b.uploads = append(uploads, b.uploads...)
	}
	if len(logs) > 0 {
		b.logs = append(logs, b.logs...)
	}
	b.mu.Unlock()
}

// close stops the flusher and drains. Uses a detached context with a
// short timeout for the final flush so a cancelled run still persists
// what it managed to upload. Retries once on transient errors before
// giving up so a flaky DB write doesn't lose already-uploaded rows.
func (b *writeBuffer) close() error {
	close(b.stop)
	<-b.done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := b.flush(ctx)
	if err != nil {
		slog.Warn("write buffer final flush failed, retrying", "err", err)
		err = b.flush(ctx)
	}
	return err
}
