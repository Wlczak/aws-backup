package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Wlczak/aws-backup/internal/db"
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
	if err != nil {
		writeError(w, http.StatusNotFound, err)
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

type triggerRunResponse struct {
	RunID int64 `json:"run_id"`
}

func (s *Server) handleTriggerRun(w http.ResponseWriter, r *http.Request) {
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

	eng, err := s.deps.BuildEngine()
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

	go func() {
		_ = eng.RunWithID(runCtx, runID)
		s.runMu.Lock()
		s.currentRun = 0
		s.currentRunCancel = nil
		s.runMu.Unlock()
	}()

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
	cancel()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

type statusResponse struct {
	Current *runSummary `json:"current"`
	Last    *runSummary `json:"last"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.runMu.Lock()
	current := s.currentRun
	s.runMu.Unlock()

	resp := statusResponse{}
	if current > 0 {
		run, err := s.deps.DB.GetRun(r.Context(), current)
		if err == nil {
			sum := toSummary(run)
			resp.Current = &sum
		}
	}

	runs, _, err := s.deps.DB.ListRuns(r.Context(), 1, 1)
	if err == nil && len(runs) > 0 {
		sum := toSummary(runs[0])
		resp.Last = &sum
	}
	writeJSON(w, http.StatusOK, resp)
}

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
