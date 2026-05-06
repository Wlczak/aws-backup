// Package scanner implements the on-demand restore-status reconciliation
// path. It takes a list of S3 keys, fans out HeadObject across a fixed
// worker pool, parses the x-amz-restore header on each response, and
// updates the local files index accordingly.
//
// Two entry points exist:
//
//   - RunFull walks every distinct s3_key in the index — individually
//     uploaded files and zip archives alike — and issues a HEAD per
//     object. This is the authoritative reconciliation used to recover
//     from missed SQS notifications or to seed restore state when the
//     SQS subscription was added late.
//
//   - RunPending walks only files whose local restore_status is
//     "in_progress". Cheap; safe to run after every SQS drain to catch
//     completion events that never reached the queue.
//
// Both publish restore_scan_* events on the engine bus so the SSE layer
// surfaces progress in the UI without any handler-side wiring.
package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/events"
	"github.com/Wlczak/aws-backup/internal/storage"
)

// WorkerCount is the size of the HEAD-fanout pool. 16 keeps the wall
// time reasonable on large indexes without hammering S3 hard enough to
// trigger 503/SlowDown — request-rate limits on a single prefix start
// well above this.
const WorkerCount = 16

// progressEmitEvery throttles the cadence of restore_scan_progress
// events so a 100k-file scan doesn't spam the SSE channel with 100k
// events. Final completion is always emitted regardless.
const progressEmitEvery = 50

// DB is the slice of *db.DB the scanner needs.
type DB interface {
	ListAllS3Keys(ctx context.Context) ([]string, error)
	ListFilesByRestoreStatus(ctx context.Context, status string) ([]string, error)
	MarkRestoreInProgress(ctx context.Context, s3Key string) (int64, error)
	MarkRestored(ctx context.Context, s3Key string, expiresAt time.Time) (int64, error)
	ClearRestoreStatus(ctx context.Context, s3Key string) (int64, error)
}

// Mode labels a scan run.
type Mode string

const (
	ModeFull      Mode = "full"
	ModePending   Mode = "pending"
	ModeInventory Mode = "inventory"
)

// Result is the per-run summary the API returns to the UI.
type Result struct {
	Mode     Mode          `json:"mode"`
	Scanned  int           `json:"scanned"`
	Updated  int           `json:"updated"`
	Errors   int           `json:"errors"`
	Duration time.Duration `json:"duration_ns"`
}

// Scanner runs restore-status HEAD reconciliation. Construct via New.
type Scanner struct {
	db      DB
	storage func() storage.Storage // live snapshot per request
	bus     *events.Bus
	logger  *slog.Logger

	// running serialises overlapping invocations so a manual "Full scan"
	// can't kick off while a post-drain "Pending scan" is mid-flight,
	// which would otherwise issue duplicate HEADs and (worse) racing DB
	// writes.
	running atomic.Bool
}

// New builds a Scanner. storageFn is taken as a closure so a settings
// hot-swap is observed on every Run* call without restart. logger is
// optional.
func New(db DB, storageFn func() storage.Storage, bus *events.Bus, logger *slog.Logger) *Scanner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scanner{db: db, storage: storageFn, bus: bus, logger: logger}
}

// ErrBusy is returned when a scan is already running. The CLI / handler
// should surface this as 409 to the UI.
var ErrBusy = errors.New("scanner: another restore scan is already running")

// RunFull HEADs every individually-uploaded file and updates restore
// status accordingly.
func (s *Scanner) RunFull(ctx context.Context) (Result, error) {
	keys, err := s.db.ListAllS3Keys(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("list keys: %w", err)
	}
	return s.run(ctx, ModeFull, keys)
}

// RunPending HEADs only files marked in_progress. Cheap and idempotent;
// callers may invoke after every SQS drain to recover from missed
// completion notifications.
func (s *Scanner) RunPending(ctx context.Context) (Result, error) {
	keys, err := s.db.ListFilesByRestoreStatus(ctx, "in_progress")
	if err != nil {
		return Result{}, fmt.Errorf("list pending keys: %w", err)
	}
	return s.run(ctx, ModePending, keys)
}

// RunKeys HEADs the supplied keys with the given mode label. Used by
// the inventory-driven path so the inventory manager can enumerate
// candidate keys cheaply (from a generated CSV manifest) and reuse the
// scanner's worker pool + event emission.
func (s *Scanner) RunKeys(ctx context.Context, mode Mode, keys []string) (Result, error) {
	return s.run(ctx, mode, keys)
}

func (s *Scanner) run(ctx context.Context, mode Mode, keys []string) (Result, error) {
	if !s.running.CompareAndSwap(false, true) {
		return Result{}, ErrBusy
	}
	defer s.running.Store(false)

	st := s.storage()
	if st == nil {
		return Result{}, errors.New("storage not configured")
	}

	res := Result{Mode: mode}
	start := time.Now()
	total := len(keys)

	s.publish(engine.Event{
		Type: engine.EventRestoreScanStart,
		At:   start,
		Data: map[string]any{"mode": string(mode), "total": total},
	})

	if total == 0 {
		res.Duration = time.Since(start)
		s.publish(engine.Event{
			Type: engine.EventRestoreScanComplete,
			At:   time.Now(),
			Data: map[string]any{
				"mode":    string(mode),
				"scanned": 0,
				"updated": 0,
				"errors":  0,
			},
		})
		return res, nil
	}

	jobs := make(chan string)
	var (
		mu      sync.Mutex
		scanned int
		updated int
		errCnt  int
		wg      sync.WaitGroup
	)

	for i := 0; i < WorkerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range jobs {
				if ctx.Err() != nil {
					return
				}
				n, hadErr := s.applyKey(ctx, st, key)
				mu.Lock()
				scanned++
				if hadErr {
					errCnt++
				}
				updated += n
				cur := scanned
				curUpdated := updated
				curErr := errCnt
				mu.Unlock()
				if cur%progressEmitEvery == 0 {
					s.publish(engine.Event{
						Type: engine.EventRestoreScanProgress,
						At:   time.Now(),
						Data: map[string]any{
							"mode":    string(mode),
							"scanned": cur,
							"updated": curUpdated,
							"errors":  curErr,
							"total":   total,
						},
					})
				}
			}
		}()
	}

	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		defer close(jobs)
		for _, k := range keys {
			select {
			case <-ctx.Done():
				return
			case jobs <- k:
			}
		}
	}()
	wg.Wait()
	<-feedDone

	res.Scanned = scanned
	res.Updated = updated
	res.Errors = errCnt
	res.Duration = time.Since(start)

	if err := ctx.Err(); err != nil {
		s.publish(engine.Event{
			Type: engine.EventRestoreScanFailed,
			At:   time.Now(),
			Data: map[string]any{"mode": string(mode), "error": err.Error()},
		})
		return res, err
	}

	s.publish(engine.Event{
		Type: engine.EventRestoreScanComplete,
		At:   time.Now(),
		Data: map[string]any{
			"mode":    string(mode),
			"scanned": res.Scanned,
			"updated": res.Updated,
			"errors":  res.Errors,
		},
	})
	return res, nil
}

// applyKey HEADs one object, parses its x-amz-restore header, and writes
// the resulting state to the DB. Returns the count of rows written and a
// flag indicating an error so the caller can update its tally without
// another lock round.
func (s *Scanner) applyKey(ctx context.Context, st storage.Storage, key string) (int, bool) {
	head, err := st.Head(ctx, key)
	if err != nil {
		// A missing object during a full scan is informational, not an
		// error — the file may have been deleted from S3 by an operator
		// outside the app. Don't pollute the error count with these.
		if errors.Is(err, storage.ErrNotFound) {
			return 0, false
		}
		s.logger.Warn("restore scan head failed", "key", key, "err", err)
		return 0, true
	}

	state, expiry := parseRestoreHeader(head.Restore)
	switch state {
	case restoreInProgress:
		n, err := s.db.MarkRestoreInProgress(ctx, key)
		if err != nil {
			s.logger.Warn("restore scan mark in_progress failed", "key", key, "err", err)
			return 0, true
		}
		return int(n), false
	case restoreCompleted:
		if expiry.IsZero() {
			// Without an expiry we'd lose the column's meaning; treat
			// as a soft error rather than write an inconsistent row.
			s.logger.Warn("restore header missing expiry-date", "key", key, "raw", head.Restore)
			return 0, true
		}
		n, err := s.db.MarkRestored(ctx, key, expiry)
		if err != nil {
			s.logger.Warn("restore scan mark restored failed", "key", key, "err", err)
			return 0, true
		}
		return int(n), false
	}
	// No x-amz-restore header: object isn't currently restored. Clear any
	// stale 'in_progress' / 'restored' rows so an expired temporary copy
	// doesn't linger in the index forever.
	n, err := s.db.ClearRestoreStatus(ctx, key)
	if err != nil {
		s.logger.Warn("restore scan clear status failed", "key", key, "err", err)
		return 0, true
	}
	return int(n), false
}

func (s *Scanner) publish(ev engine.Event) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(ev)
}

type restoreState int

const (
	restoreNone restoreState = iota
	restoreInProgress
	restoreCompleted
)

// parseRestoreHeader interprets the S3 x-amz-restore header. Format:
//
//	ongoing-request="true"
//	ongoing-request="false", expiry-date="Fri, 21 Dec 2012 00:00:00 GMT"
//
// The expiry timestamp uses RFC1123 with a fixed "GMT" zone (the same
// format http.ParseTime accepts).
func parseRestoreHeader(raw string) (restoreState, time.Time) {
	if raw == "" {
		return restoreNone, time.Time{}
	}
	parts := splitHeader(raw)
	ongoing, ok := parts["ongoing-request"]
	if !ok {
		return restoreNone, time.Time{}
	}
	if ongoing == "true" {
		return restoreInProgress, time.Time{}
	}
	if ongoing != "false" {
		return restoreNone, time.Time{}
	}
	expRaw, ok := parts["expiry-date"]
	if !ok {
		return restoreCompleted, time.Time{}
	}
	if t, err := http.ParseTime(expRaw); err == nil {
		return restoreCompleted, t
	}
	if t, err := time.Parse(time.RFC1123, expRaw); err == nil {
		return restoreCompleted, t
	}
	return restoreCompleted, time.Time{}
}

// splitHeader splits a `key="value", key="value"` header into a map,
// stripping surrounding quotes. Robust to extra spaces around commas.
func splitHeader(h string) map[string]string {
	out := map[string]string{}
	rest := h
	for len(rest) > 0 {
		// Find next key=
		eq := -1
		for i := 0; i < len(rest); i++ {
			if rest[i] == '=' {
				eq = i
				break
			}
		}
		if eq < 0 {
			break
		}
		key := trimSpace(rest[:eq])
		rest = rest[eq+1:]
		if len(rest) == 0 {
			break
		}
		// Value is quoted "..." possibly followed by ", "
		if rest[0] != '"' {
			break
		}
		rest = rest[1:]
		end := -1
		for i := 0; i < len(rest); i++ {
			if rest[i] == '"' {
				end = i
				break
			}
		}
		if end < 0 {
			break
		}
		out[key] = rest[:end]
		rest = rest[end+1:]
		// Skip optional ", "
		for len(rest) > 0 && (rest[0] == ',' || rest[0] == ' ') {
			rest = rest[1:]
		}
	}
	return out
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}
