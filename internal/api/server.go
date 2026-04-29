package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/events"
	"github.com/Wlczak/aws-backup/internal/storage"
	webassets "github.com/Wlczak/aws-backup/web"
)

// Deps holds everything the HTTP handlers need.
type Deps struct {
	DB         *db.DB
	Bus        *events.Bus
	Config     *config.Config
	ConfigPath string
	// BuildEngine constructs an Engine for a new backup run with the
	// current config. mode and scanPaths are per-run parameters: mode
	// selects scan-only, upload-only, or full (default); scanPaths
	// restricts a scan to matching file paths (partial rescan).
	BuildEngine func(mode engine.RunMode, scanPaths []string) (*engine.Engine, error)
	// ApplySettings fires after a successful PUT /api/settings save.
	// It is passed the previous and the newly saved config so the
	// caller can hot-swap source/storage/scheduler. Returning an error
	// rolls back the save. nil means "nothing to apply" (tests).
	ApplySettings func(prev, next config.Config) error
	// Storage returns the live storage handle. Each handler invocation
	// calls this so a settings hot-swap (PUT /api/settings) is picked up
	// without a service restart — Deps used to cache the storage by
	// value, which left sync/restore handlers calling the old endpoint
	// after the user changed config. (#131)
	Storage       func() storage.Storage
	StoragePrefix string
	// SyncDBToS3, when non-nil, is called after a backup run when the user
	// either let it complete, clicked Stop, or clicked Cancel. The reason
	// argument ("complete" | "stop" | "cancel") lets the implementation
	// label its progress events so the dashboard can show which trigger
	// produced the upload. (#128)
	SyncDBToS3 func(ctx context.Context, runID int64, reason string) error
	// SyncRestoreStatus, when non-nil, drains the configured SQS queue of
	// pending S3 Glacier restore events and applies them to the DB.
	// Returns the number of messages processed. nil means "SQS not
	// configured" — the handler returns 503 so the UI can surface that.
	SyncRestoreStatus func(ctx context.Context) (int, error)
	Logger            *slog.Logger
}

// Server exposes the Router for tests and for the CLI to Serve() from.
type Server struct {
	deps Deps

	// runMu guards currentRun + currentRunCancel so we can expose "is a
	// run in progress?" to /api/status and allow /api/runs/:id/cancel.
	runMu            sync.Mutex
	currentRun       int64 // 0 when idle
	currentRunCancel context.CancelFunc
	// currentRunStopReq is the graceful-stop flag for the in-flight run.
	// /api/runs/:id/stop sets it; the engine polls via IsStopRequested
	// between files / groups and exits cleanly with status="stopped"
	// once the current upload finishes. Cleared at run start. (#124)
	currentRunStopReq atomic.Bool
	// currentRunCancelReq distinguishes a user-initiated /cancel from a
	// service-shutdown cancel: handleCancelRun sets it before calling
	// currentRunCancel, Server.Shutdown does not. The post-run goroutine
	// reads it to decide whether to upload the DB to S3 after a cancelled
	// run. Cleared at run start. (#128)
	currentRunCancelReq atomic.Bool
	// runWg tracks engine goroutines spawned by handleTriggerRun so the
	// CLI can wait for them on shutdown before tearing down DB / storage.
	runWg sync.WaitGroup
	// shutdownCh is closed once at the top of Shutdown. The post-run
	// DB-sync goroutine watches it so an in-flight DB upload aborts
	// promptly when the service is shutting down — otherwise a 600 s
	// timeout-bound sync would outlive Server.Shutdown's 10 s budget and
	// race app.close() tearing down DB/storage. (#128)
	shutdownCh   chan struct{}
	shutdownOnce sync.Once

	// cfgMu guards reads/writes of deps.Config (the pointee) and
	// deps.StoragePrefix, both of which can be replaced by handlePutSettings.
	cfgMu sync.RWMutex

	// statsCache coalesces /api/files/stats across poll-heavy UI clients
	// so a full-table COUNT/SUM doesn't hit the DB more than once every
	// statsCacheTTL.
	statsMu     sync.Mutex
	statsValue  db.FileStats
	statsExpiry time.Time
}

// snapshotConfig returns a copy of the live config, plus a "loaded" flag.
// Callers should never share the returned value across the lock; mutate-
// then-write paths must use updateConfig.
func (s *Server) snapshotConfig() (config.Config, bool) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.deps.Config == nil {
		return config.Config{}, false
	}
	return *s.deps.Config, true
}

// storagePrefix returns the live S3 key prefix.
func (s *Server) storagePrefix() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.deps.StoragePrefix
}

// storage returns the live storage handle, or nil when none is wired.
// Handlers should snapshot this once per request and use the local for
// the rest of their work so a concurrent settings hot-swap doesn't
// surface a half-old, half-new client mid-handler. (#131)
func (s *Server) storage() storage.Storage {
	if s.deps.Storage == nil {
		return nil
	}
	return s.deps.Storage()
}

// updateConfig atomically replaces both deps.Config (pointee) and
// deps.StoragePrefix so concurrent readers always see a consistent pair.
func (s *Server) updateConfig(c config.Config) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.deps.Config != nil {
		*s.deps.Config = c
	}
	s.deps.StoragePrefix = c.S3.KeyPrefix
}

// setStoragePrefix is the rollback-only path for handlePutSettings; updateConfig
// covers the success path.
func (s *Server) setStoragePrefix(p string) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	s.deps.StoragePrefix = p
}

// Shutdown cancels any in-flight engine run and waits for the run goroutine
// to finish (bounded by ctx). Call before tearing down DB / Storage so the
// engine doesn't hit closed handles mid-write.
func (s *Server) Shutdown(ctx context.Context) error {
	// Close shutdownCh first so any in-flight DB-sync goroutine aborts
	// its upload before we cancel the run ctx; otherwise the sync's
	// 600 s timeout could outlive this call. (#128)
	s.shutdownOnce.Do(func() {
		if s.shutdownCh != nil {
			close(s.shutdownCh)
		}
	})

	s.runMu.Lock()
	if s.currentRunCancel != nil {
		s.currentRunCancel()
	}
	s.runMu.Unlock()

	done := make(chan struct{})
	go func() {
		s.runWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// statsCacheTTL bounds staleness of the cached /api/files/stats response.
// Short enough that the dashboard feels live, long enough that a tight
// poll loop can't pin a 300k-row index in scan.
const statsCacheTTL = 2 * time.Second

// NewServer wires up a *Server with validated Deps.
func NewServer(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Server{deps: d, shutdownCh: make(chan struct{})}
}

// Router builds the chi router with all /api/* routes mounted.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)

	r.Route("/api", func(r chi.Router) {
		r.Get("/status", s.handleStatus)

		r.Get("/runs", s.handleListRuns)
		r.Post("/runs", s.handleTriggerRun)
		r.Get("/runs/{id}", s.handleGetRun)
		r.Post("/runs/{id}/cancel", s.handleCancelRun)
		r.Post("/runs/{id}/stop", s.handleStopRun)
		r.Post("/runs/{id}/continue", s.handleContinueRun)

		r.Get("/files", s.handleListFiles)
		r.Get("/files/stats", s.handleFileStats)
		r.Post("/files/retry", s.handleRetryFiles)
		r.Post("/files/purge-missing", s.handlePurgeMissingFiles)
		r.Delete("/files", s.handleDeleteFiles)
		r.Post("/files/{id}/retry", s.handleRetryFile)
		r.Delete("/files/{id}", s.handleDeleteFile)

		r.Get("/settings", s.handleGetSettings)
		r.Put("/settings", s.handlePutSettings)

		r.Get("/smb/test", s.handleTestSource)
		r.Get("/s3/test", s.handleTestStorage)

		r.Post("/restore/estimate", s.handleRestoreEstimate)
		r.Post("/restore/trigger", s.handleRestoreTrigger)
		r.Post("/restore/sync-status", s.handleRestoreSyncStatus)

		r.Post("/sync", s.handleSync)
		r.Post("/sync/full", s.handleSyncFull)
		r.Post("/sync/delete-cloud-paths", s.handleDeleteCloudPaths)

		r.Mount("/events", sseHandler(s.deps.Bus, s.deps.Logger, s.sseReplay))
	})

	// Serve the embedded Svelte SPA at "/". Any path that doesn't resolve
	// under web/dist falls back to index.html so Vite's hash router keeps
	// working across refreshes and deep links.
	if sub, err := fs.Sub(webassets.Dist, "dist"); err == nil {
		r.Mount("/", spaHandler(sub))
	}

	return r
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a simple {"error":"..."} body.
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// errBadJSON and errMissingBody help return consistent 400s.
var errBadJSON = errors.New("invalid JSON body")

// IsStopRequested returns whether the in-flight run has been asked to
// stop gracefully. The CLI wires this into engine.Options.StopRequested
// so the engine exits cleanly between files when the flag flips.
func (s *Server) IsStopRequested() bool {
	return s.currentRunStopReq.Load()
}

// sseReplay is passed to sseHandler as the replay callback. On each SSE
// connect it returns a burst of engine.Events that reconstruct the
// in-flight run's history from the DB so clients that refresh mid-run
// see the full log rather than starting blank. (#130)
func (s *Server) sseReplay(ctx context.Context) []engine.Event {
	s.runMu.Lock()
	runID := s.currentRun
	s.runMu.Unlock()
	if runID == 0 {
		return nil
	}

	run, err := s.deps.DB.GetRun(ctx, runID)
	if err != nil {
		return nil
	}

	evts := []engine.Event{
		{
			Type:  engine.EventRunStart,
			RunID: run.ID,
			At:    run.StartedAt,
			Data: map[string]any{
				"files_scanned":  run.FilesScanned,
				"files_uploaded": run.FilesUploaded,
				"bytes_uploaded": run.BytesUploaded,
			},
		},
	}

	logs, err := s.deps.DB.ListLogs(ctx, runID)
	if err != nil {
		return evts
	}
	for _, l := range logs {
		evts = append(evts, engine.Event{
			Type:  engine.EventRunLog,
			RunID: l.RunID,
			At:    l.Timestamp,
			Data:  map[string]any{"level": l.Level, "message": l.Message},
		})
	}
	return evts
}
