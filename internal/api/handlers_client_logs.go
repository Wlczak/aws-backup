package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
)

type clientLogEntryRequest struct {
	Timestamp time.Time      `json:"timestamp"`
	Level     string         `json:"level"`
	Source    string         `json:"source"`
	Message   string         `json:"message"`
	Route     string         `json:"route,omitempty"`
	URL       string         `json:"url,omitempty"`
	Stack     string         `json:"stack,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Context   map[string]any `json:"context,omitempty"`
}

type clientLogsRequest struct {
	Entries []clientLogEntryRequest `json:"entries"`
}

type clientLogResponse struct {
	ID         int64          `json:"id"`
	Timestamp  time.Time      `json:"timestamp"`
	ReceivedAt time.Time      `json:"received_at"`
	Level      string         `json:"level"`
	Source     string         `json:"source"`
	Message    string         `json:"message"`
	Route      string         `json:"route,omitempty"`
	URL        string         `json:"url,omitempty"`
	Stack      string         `json:"stack,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	Context    map[string]any `json:"context,omitempty"`
}

type clientLogsListResponse struct {
	Logs  []clientLogResponse `json:"logs"`
	Total int64               `json:"total"`
	Page  int                 `json:"page"`
	Limit int                 `json:"limit"`
}

func toClientLogResponse(row db.ClientLog) clientLogResponse {
	return clientLogResponse{
		ID:         row.ID,
		Timestamp:  row.RecordedAt,
		ReceivedAt: row.ReceivedAt,
		Level:      row.Level,
		Source:     row.Source,
		Message:    row.Message,
		Route:      row.Route,
		URL:        row.URL,
		Stack:      row.Stack,
		SessionID:  row.SessionID,
		Context:    row.Context,
	}
}

func normalizeClientLogLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return "debug"
	case "info":
		return "info"
	case "warn", "warning":
		return "warn"
	case "error":
		return "error"
	default:
		return ""
	}
}

func (s *Server) handlePostClientLogs(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength == 0 {
		writeError(w, http.StatusBadRequest, errors.New("request body is required"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var req clientLogsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	if len(req.Entries) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("entries are required"))
		return
	}
	if len(req.Entries) > 100 {
		writeError(w, http.StatusBadRequest, errors.New("too many log entries"))
		return
	}
	entries := make([]db.ClientLogEntry, 0, len(req.Entries))
	for i, e := range req.Entries {
		level := normalizeClientLogLevel(e.Level)
		source := strings.TrimSpace(e.Source)
		message := strings.TrimSpace(e.Message)
		if level == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("entry %d: invalid level %q", i, e.Level))
			return
		}
		if source == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("entry %d: source is required", i))
			return
		}
		if message == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("entry %d: message is required", i))
			return
		}
		entries = append(entries, db.ClientLogEntry{
			RecordedAt: e.Timestamp,
			Level:      level,
			Source:     source,
			Message:    message,
			Route:      strings.TrimSpace(e.Route),
			URL:        strings.TrimSpace(e.URL),
			Stack:      e.Stack,
			SessionID:  strings.TrimSpace(e.SessionID),
			Context:    e.Context,
		})
	}
	if err := s.deps.DB.AppendClientLogs(r.Context(), entries); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, affectedResponse{Affected: int64(len(entries))})
}

func (s *Server) handleListClientLogs(w http.ResponseWriter, r *http.Request) {
	page := intParam(r, "page", 1)
	limit := intParam(r, "limit", 100)
	logs, total, err := s.deps.DB.ListClientLogs(r.Context(), page, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]clientLogResponse, 0, len(logs))
	for _, row := range logs {
		out = append(out, toClientLogResponse(row))
	}
	writeJSON(w, http.StatusOK, clientLogsListResponse{Logs: out, Total: total, Page: page, Limit: limit})
}

func (s *Server) handleDeleteClientLogs(w http.ResponseWriter, r *http.Request) {
	n, err := s.deps.DB.DeleteClientLogs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, affectedResponse{Affected: n})
}
