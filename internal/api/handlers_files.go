package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Wlczak/aws-backup/internal/db"
)

type filesListResponse struct {
	Files []fileEntry `json:"files"`
	Total int64       `json:"total"`
	Page  int         `json:"page"`
	Limit int         `json:"limit"`
}

type fileEntry struct {
	ID         int64     `json:"id"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	MTime      time.Time `json:"mtime"`
	MD5        string    `json:"md5,omitempty"`
	Status     string    `json:"status"`
	ZipName    string    `json:"zip_name,omitempty"`
	S3Key      string    `json:"s3_key,omitempty"`
	UploadedAt time.Time `json:"uploaded_at,omitempty"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	all := q.Get("all") == "true" || q.Get("all") == "1"
	page := intParam(r, "page", 1)
	limit := intParam(r, "limit", 50)
	files, total, err := s.deps.DB.ListFiles(r.Context(), db.FilesFilter{
		Status: q.Get("status"),
		Search: q.Get("search"),
		Page:   page,
		Limit:  limit,
		All:    all,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]fileEntry, 0, len(files))
	for _, f := range files {
		out = append(out, fileEntry{
			ID: f.ID, Path: f.Path, Size: f.Size, MTime: f.MTime, MD5: f.MD5,
			Status: f.Status, ZipName: f.ZipName, S3Key: f.S3Key,
			UploadedAt: f.UploadedAt, LastSeenAt: f.LastSeenAt,
		})
	}
	if all {
		// Report the real count so the client can page in tree mode if
		// it chooses, but the payload already contains everything.
		page = 1
		limit = len(out)
	}
	writeJSON(w, http.StatusOK, filesListResponse{
		Files: out, Total: total,
		Page:  page,
		Limit: limit,
	})
}

type fileStatsResponse struct {
	ByStatus   map[string]int64 `json:"by_status"`
	TotalCount int64            `json:"total_count"`
	TotalSize  int64            `json:"total_size"`
}

func (s *Server) handleFileStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.cachedStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, fileStatsResponse{
		ByStatus: st.ByStatus, TotalCount: st.TotalCount, TotalSize: st.TotalSize,
	})
}

// cachedStats serves /api/files/stats from a short TTL cache so tight
// dashboard polls don't rescan the whole files table per request.
// The mutex is held for the duration of the DB call to prevent a stampede
// where many goroutines all observe a stale expiry and fire concurrent queries.
func (s *Server) cachedStats(ctx context.Context) (db.FileStats, error) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if time.Now().Before(s.statsExpiry) {
		return s.statsValue, nil
	}
	st, err := s.deps.DB.Stats(ctx)
	if err != nil {
		return db.FileStats{}, err
	}
	s.statsValue = st
	s.statsExpiry = time.Now().Add(statsCacheTTL)
	return st, nil
}

type idsRequest struct {
	IDs       []int64  `json:"ids"`
	AllFailed bool     `json:"all_failed"`
	Paths     []string `json:"paths"`
}

type affectedResponse struct {
	Affected int64 `json:"affected"`
}

// handleRetryFile flips a single file back to 'pending' so the next run
// (or a manually triggered one) re-uploads it.
func (s *Server) handleRetryFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid file id"))
		return
	}
	n, err := s.deps.DB.MarkPendingByIDs(r.Context(), []int64{id})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, errors.New("no such file"))
		return
	}
	writeJSON(w, http.StatusOK, affectedResponse{Affected: n})
}

// handleRetryFiles is the bulk variant. Body: {ids: []} or {all_failed: true}.
func (s *Server) handleRetryFiles(w http.ResponseWriter, r *http.Request) {
	var req idsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	var (
		affected int64
		err      error
	)
	switch {
	case req.AllFailed:
		affected, err = s.deps.DB.MarkAllFailedPending(r.Context())
	case len(req.IDs) > 0:
		affected, err = s.deps.DB.MarkPendingByIDs(r.Context(), req.IDs)
	case len(req.Paths) > 0:
		affected, err = s.deps.DB.MarkPendingByPaths(r.Context(), req.Paths)
	default:
		writeError(w, http.StatusBadRequest, errors.New("provide ids, paths, or all_failed"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, affectedResponse{Affected: affected})
}

// handleDeleteFile removes a row from the index. Does not touch S3.
func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid file id"))
		return
	}
	n, err := s.deps.DB.DeleteFiles(r.Context(), []int64{id})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, errors.New("no such file"))
		return
	}
	writeJSON(w, http.StatusOK, affectedResponse{Affected: n})
}

// handlePurgeMissingFiles deletes every DB row with status='missing'.
// These are files that were removed from the local source and have no
// remaining presence in S3 to restore from.
func (s *Server) handlePurgeMissingFiles(w http.ResponseWriter, r *http.Request) {
	n, err := s.deps.DB.PurgeMissingFiles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, affectedResponse{Affected: n})
}

// handleDeleteFiles is the bulk variant. Body: {ids: []}.
func (s *Server) handleDeleteFiles(w http.ResponseWriter, r *http.Request) {
	var req idsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("ids is required"))
		return
	}
	n, err := s.deps.DB.DeleteFiles(r.Context(), req.IDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, affectedResponse{Affected: n})
}
