package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/engine"
)

type runsListResponse struct {
	Runs  []runSummary `json:"runs"`
	Total int64        `json:"total"`
	Page  int          `json:"page"`
	Limit int          `json:"limit"`
}

type runSummary struct {
	ID             int64     `json:"id"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	Status         string    `json:"status"`
	FilesScanned   int64     `json:"files_scanned"`
	FilesUploaded  int64     `json:"files_uploaded"`
	BytesUploaded  int64     `json:"bytes_uploaded"`
	ErrorMessage   string    `json:"error_message,omitempty"`
}

func toSummary(r db.Run) runSummary {
	return runSummary{
		ID:            r.ID,
		StartedAt:     r.StartedAt,
		FinishedAt:    r.FinishedAt,
		Status:        r.Status,
		FilesScanned:  r.FilesScanned,
		FilesUploaded: r.FilesUploaded,
		BytesUploaded: r.BytesUploaded,
		ErrorMessage:  r.ErrorMessage,
	}
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	page := intParam(r, "page", 1)
	limit := intParam(r, "limit", 20)
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	runs, total, err := s.deps.DB.ListRuns(r.Context(), page, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]runSummary, 0, len(runs))
	for _, rn := range runs {
		out = append(out, toSummary(rn))
	}
	writeJSON(w, http.StatusOK, runsListResponse{Runs: out, Total: total, Page: page, Limit: limit})
}

type runDetailResponse struct {
	Run  runSummary    `json:"run"`
	Logs []logEntry    `json:"logs"`
}

type logEntry struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid run id"))
		return
	}
	run, err := s.deps.DB.GetRun(r.Context(), id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeError(w, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	logs, err := s.deps.DB.ListLogs(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]logEntry, 0, len(logs))
	for _, l := range logs {
		out = append(out, logEntry{ID: l.ID, Timestamp: l.Timestamp, Level: l.Level, Message: l.Message})
	}
	writeJSON(w, http.StatusOK, runDetailResponse{Run: toSummary(run), Logs: out})
}

type triggerRunRequest struct {
	// Mode controls which phases run: "full" (default), "scan", or "upload".
	Mode  string   `json:"mode"`
	// Paths restricts a scan-mode run to specific file/folder paths (partial rescan).
	Paths []string `json:"paths"`
}

type triggerRunResponse struct {
	RunID int64 `json:"run_id"`
}

func (s *Server) handleTriggerRun(w http.ResponseWriter, r *http.Request) {
	// Body is optional: an empty POST still triggers a full run.
	var req triggerRunRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
			return
		}
	}

	mode := engine.RunMode(req.Mode)
	if mode == "" {
		mode = engine.RunModeFull
	}
	switch mode {
	case engine.RunModeFull, engine.RunModeScan, engine.RunModeUpload:
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown mode %q; use full, scan, or upload", mode))
		return
	}

	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.currentRun != 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":          "a backup run is already in progress",
			"current_run_id": s.currentRun,
		})
		return
	}
	if s.deps.BuildEngine == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("engine not configured"))
		return
	}

	eng, err := s.deps.BuildEngine(mode, req.Paths)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("build engine: %w", err))
		return
	}

	// Pre-create the run row synchronously so the response body can carry
	// the run ID and /api/runs/{id}/cancel has a target immediately.
	runID, err := s.deps.DB.CreateRun(r.Context(), time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("create run: %w", err))
		return
	}

	runCtx, cancel := context.WithCancel(context.Background())
	s.currentRun = runID
	s.currentRunCancel = cancel
	// Clear any stale graceful-stop flag from a prior run before the
	// engine starts polling it. (#124)
	s.currentRunStopReq.Store(false)
	// Same for the cancel-request flag — its only role is to tell the
	// post-run goroutine "the run ended via /cancel, sync the DB", and
	// it must not survive into the next run. (#128)
	s.currentRunCancelReq.Store(false)

	// Ensure currentRun is cleared if we panic before the goroutine launches.
	// The goroutine itself is responsible for clearing it on normal completion.
	goroutineLaunched := false
	defer func() {
		if !goroutineLaunched {
			s.currentRun = 0
			s.currentRunCancel = nil
			cancel()
		}
	}()

	syncDBToS3 := s.deps.SyncDBToS3
	logger := s.deps.Logger
	s.runWg.Add(1)
	go func() {
		defer s.runWg.Done()
		// Release the run context regardless of how RunWithID returned, so
		// the WithCancel chain doesn't leak goroutines/timers per run.
		defer cancel()
		runErr := eng.RunWithID(runCtx, runID)
		// Read the stop/cancel flags BEFORE clearing currentRun so a
		// concurrent handleTriggerRun (which clears them under runMu)
		// can't race us to false. (#128)
		stopReq := s.currentRunStopReq.Load()
		cancelReq := s.currentRunCancelReq.Load()
		s.runMu.Lock()
		s.currentRun = 0
		s.currentRunCancel = nil
		s.runMu.Unlock()
		s.maybeSyncDBToS3(syncDBToS3, logger, runID, mode, runErr, stopReq, cancelReq)
	}()
	goroutineLaunched = true

	writeJSON(w, http.StatusAccepted, triggerRunResponse{RunID: runID})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid run id"))
		return
	}
	s.runMu.Lock()
	cancel := s.currentRunCancel
	current := s.currentRun
	s.runMu.Unlock()
	if cancel == nil || current != id {
		writeError(w, http.StatusNotFound, errors.New("no running run with that id"))
		return
	}
	// Mark this as a user-initiated cancel so the post-run goroutine
	// distinguishes it from Server.Shutdown's cancel and uploads the DB
	// to S3 with "cancel" reason + 30 s timeout. (#128)
	s.currentRunCancelReq.Store(true)
	cancel()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

// dbSyncStopTimeout bounds the post-Stop and post-completion DB upload.
// 600 s comfortably covers a hundreds-of-MB index over modest links and
// also caps the duration runWg can hold up Server.Shutdown.
const dbSyncStopTimeout = 600 * time.Second

// maybeSyncDBToS3 picks a reason and timeout based on how the run ended,
// then runs the sync in a context that aborts on Server.Shutdown so an
// in-flight upload doesn't outlive app.close() tearing down DB/storage.
// Returns immediately when the run failed or was cancelled by service
// shutdown — those branches are explicitly out of scope for #128. Also
// skips scan-only runs: the scan phase doesn't change S3, so re-uploading
// the index would just churn versions on a versioned bucket without the
// reconcile data the sidecar exists to provide.
func (s *Server) maybeSyncDBToS3(
	sync func(ctx context.Context, runID int64, reason string) error,
	logger *slog.Logger,
	runID int64,
	mode engine.RunMode,
	runErr error,
	stopReq, cancelReq bool,
) {
	if sync == nil {
		return
	}
	if mode == engine.RunModeScan {
		return
	}

	var (
		reason  string
		timeout time.Duration
	)
	switch {
	case stopReq && runErr == nil:
		reason, timeout = "stop", dbSyncStopTimeout
	case !stopReq && !cancelReq && runErr == nil:
		reason, timeout = "complete", dbSyncStopTimeout
	default:
		// Force-cancel, engine failure, or service-shutdown cancel — skip.
		// Force-cancel discards in-flight work, so the partial DB state
		// would just churn the versioned index without value.
		return
	}

	// If shutdown already started, don't initiate a new upload.
	select {
	case <-s.shutdownCh:
		return
	default:
	}

	syncCtx, syncCancel := context.WithTimeout(context.Background(), timeout)
	defer syncCancel()
	// Mirror SIGINT into syncCtx so an in-flight upload aborts when the
	// service is shutting down. shutdownCh is closed exactly once at the
	// top of Server.Shutdown.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-s.shutdownCh:
			syncCancel()
		case <-done:
		}
	}()

	if err := sync(syncCtx, runID, reason); err != nil && logger != nil {
		logger.Warn("db sync to s3 failed", "error", err, "reason", reason)
	}
}

// handleStopRun signals a graceful stop: the engine finishes its
// in-flight upload and exits between files. Distinct from /cancel,
// which forcibly kills the upload mid-stream. (#124)
func (s *Server) handleStopRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid run id"))
		return
	}
	s.runMu.Lock()
	current := s.currentRun
	s.runMu.Unlock()
	if current == 0 || current != id {
		writeError(w, http.StatusNotFound, errors.New("no running run with that id"))
		return
	}
	s.currentRunStopReq.Store(true)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopping"})
}

// handleContinueRun clears a pending graceful-stop request so the run
// keeps uploading. Inverse of handleStopRun. 404 when there is no run
// in flight under the given id (avoids resurrecting a flag for a run
// that has already exited). (#124 follow-up)
func (s *Server) handleContinueRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid run id"))
		return
	}
	s.runMu.Lock()
	current := s.currentRun
	s.runMu.Unlock()
	if current == 0 || current != id {
		writeError(w, http.StatusNotFound, errors.New("no running run with that id"))
		return
	}
	s.currentRunStopReq.Store(false)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "continuing"})
}

type statusResponse struct {
	Current *runSummary `json:"current"`
	Last    *runSummary `json:"last"`
	// StopRequested is true while a graceful stop is pending: the engine
	// will exit cleanly after the in-flight upload. Lets the UI flip the
	// Stop button to a Continue affordance and render a "stopping" badge.
	// (#124 follow-up)
	StopRequested bool `json:"stop_requested"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.runMu.Lock()
	current := s.currentRun
	s.runMu.Unlock()

	resp := statusResponse{}
	if current > 0 {
		run, err := s.deps.DB.GetRun(r.Context(), current)
		if err != nil {
			// Don't return 200 with empty body — the dashboard polls this
			// every few seconds and would render a green "idle" while the
			// DB is unreachable. Surface the failure so the SPA can show
			// it. (#117)
			writeError(w, http.StatusInternalServerError, fmt.Errorf("get current run: %w", err))
			return
		}
		sum := toSummary(run)
		resp.Current = &sum
		resp.StopRequested = s.currentRunStopReq.Load()
	}

	runs, _, err := s.deps.DB.ListRuns(r.Context(), 1, 1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("list runs: %w", err))
		return
	}
	if len(runs) > 0 {
		sum := toSummary(runs[0])
		resp.Last = &sum
	}
	writeJSON(w, http.StatusOK, resp)
}

// maxPageLimit caps the per-request `limit` query parameter so a caller
// can't force the DB to materialize and the encoder to serialize huge
// result sets via `?limit=2147483647`. (#65)
const maxPageLimit = 1000

func intParam(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// decodeJSON is a small helper for PUT/POST bodies.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
