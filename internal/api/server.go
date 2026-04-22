package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"

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
	// Storage and StoragePrefix are used by the sync handler to list S3
	// objects and compare them against the DB index.
	Storage       storage.Storage
	StoragePrefix string
	Logger        *slog.Logger
}

// Server exposes the Router for tests and for the CLI to Serve() from.
type Server struct {
	deps Deps

	// runMu guards currentRun + currentRunCancel so we can expose "is a
	// run in progress?" to /api/status and allow /api/runs/:id/cancel.
	runMu             sync.Mutex
	currentRun        int64 // 0 when idle
	currentRunCancel  context.CancelFunc
}

// NewServer wires up a *Server with validated Deps.
func NewServer(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Server{deps: d}
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

		r.Get("/files", s.handleListFiles)
		r.Get("/files/stats", s.handleFileStats)
		r.Post("/files/retry", s.handleRetryFiles)
		r.Delete("/files", s.handleDeleteFiles)
		r.Post("/files/{id}/retry", s.handleRetryFile)
		r.Delete("/files/{id}", s.handleDeleteFile)

		r.Get("/settings", s.handleGetSettings)
		r.Put("/settings", s.handlePutSettings)

		r.Get("/smb/test", s.handleTestSource)
		r.Get("/s3/test", s.handleTestStorage)

		r.Post("/restore/estimate", s.handleRestoreEstimate)
		r.Post("/restore/trigger", s.handleRestoreTrigger)

		r.Post("/sync", s.handleSync)

		r.Mount("/events", sseHandler(s.deps.Bus))
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
