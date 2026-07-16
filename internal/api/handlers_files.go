package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
	ID               int64      `json:"id"`
	Path             string     `json:"path"`
	Size             int64      `json:"size"`
	MTime            time.Time  `json:"mtime"`
	MD5              string     `json:"md5,omitempty"`
	Status           string     `json:"status"`
	ZipID            *int64     `json:"zip_id,omitempty"`
	ZipName          string     `json:"zip_name,omitempty"`
	S3Key            string     `json:"s3_key,omitempty"`
	UploadedAt       time.Time  `json:"uploaded_at,omitempty"`
	LastSeenAt       time.Time  `json:"last_seen_at"`
	RestoreStatus    string     `json:"restore_status,omitempty"`
	RestoreExpiresAt *time.Time `json:"restore_expires_at,omitempty"`
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	all := q.Get("all") == "true" || q.Get("all") == "1"
	page := intParam(r, "page", 1)
	limit := intParam(r, "limit", 50)
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	// `all=true` powers the Files tree view, which needs every row in one
	// shot. Cap the result so a million-row index can't OOM the server or
	// saturate the uplink on a single anonymous request — the caller
	// must paginate normally above the cap. (#64)
	filter := db.FilesFilter{
		Status: q.Get("status"),
		Search: q.Get("search"),
		Page:   page,
		Limit:  limit,
		All:    all,
	}
	if all {
		filter.All = false
		filter.Page = 1
		filter.Limit = maxAllRows + 1
	}
	keyParams := url.Values{
		"all": {strconv.FormatBool(all)}, "status": {filter.Status}, "search": {filter.Search},
	}
	if !all {
		keyParams.Set("page", strconv.Itoa(page))
		keyParams.Set("limit", strconv.Itoa(limit))
	}
	key := "files?" + keyParams.Encode()
	database := s.deps.DB
	s.writeCachedFileJSON(w, r, database, key, func(ctx context.Context) (any, error) {
		files, total, err := database.ListFiles(ctx, filter)
		if err != nil {
			return nil, err
		}
		if all && total > maxAllRows {
			return nil, &responseStatusError{status: http.StatusBadRequest, err: fmt.Errorf(
				"index has %d rows, exceeds the %d-row cap on all=true; paginate via page/limit instead",
				total, maxAllRows,
			)}
		}
		out := make([]fileEntry, 0, len(files))
		for _, f := range files {
			out = append(out, toFileEntry(f))
		}
		responsePage, responseLimit := page, limit
		if all {
			responsePage = 1
			responseLimit = len(out)
		}
		return filesListResponse{
			Files: out, Total: total,
			Page: responsePage, Limit: responseLimit,
		}, nil
	})
}

func toFileEntry(f db.File) fileEntry {
	return fileEntry{
		ID: f.ID, Path: f.Path, Size: f.Size, MTime: f.MTime, MD5: f.MD5,
		Status: f.Status, ZipID: f.ZipID, ZipName: f.ZipName, S3Key: f.S3Key,
		UploadedAt: f.UploadedAt, LastSeenAt: f.LastSeenAt,
		RestoreStatus: f.RestoreStatus, RestoreExpiresAt: f.RestoreExpiresAt,
	}
}

type treeFolderEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	FileCount int64  `json:"file_count"`
	TotalSize int64  `json:"total_size"`
}

type treeListResponse struct {
	Prefix  string            `json:"prefix"`
	Folders []treeFolderEntry `json:"folders"`
	Files   []fileEntry       `json:"files"`
}

// handleListTree returns the immediate children of a folder prefix
// (subfolders + direct files), aggregated for the lazy tree view. Each
// click in the UI fetches one prefix; we never load the whole index.
func (s *Server) handleListTree(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	// Strip a leading slash so callers can pass `/foo/bar` or `foo/bar`
	// interchangeably (file paths are stored without a leading slash).
	for len(prefix) > 0 && (prefix[0] == '/' || prefix[0] == '\\') {
		prefix = prefix[1:]
	}
	for len(prefix) > 0 && (prefix[len(prefix)-1] == '/' || prefix[len(prefix)-1] == '\\') {
		prefix = prefix[:len(prefix)-1]
	}
	statusFilter := q.Get("status")

	key := "tree?" + url.Values{"prefix": {prefix}, "status": {statusFilter}}.Encode()
	database := s.deps.DB
	s.writeCachedFileJSON(w, r, database, key, func(ctx context.Context) (any, error) {
		folders, files, err := database.ListTreeChildren(ctx, prefix, statusFilter)
		if err != nil {
			return nil, err
		}
		resp := treeListResponse{
			Prefix: prefix, Folders: make([]treeFolderEntry, 0, len(folders)),
			Files: make([]fileEntry, 0, len(files)),
		}
		for _, fd := range folders {
			resp.Folders = append(resp.Folders, treeFolderEntry{
				Name: fd.Name, Path: fd.Path, FileCount: fd.FileCount, TotalSize: fd.TotalSize,
			})
		}
		for _, f := range files {
			resp.Files = append(resp.Files, toFileEntry(f))
		}
		return resp, nil
	})
}

type subtreeIDsResponse struct {
	IDs       []int64  `json:"ids"`
	Paths     []string `json:"paths"`
	Total     int64    `json:"total"`
	Truncated bool     `json:"truncated"`
}

// subtreeIDsMaxRows caps a single "select-folder" expansion in the
// lazy tree. 50k IDs is enough for typical folders without making one
// click pull millions of rows.
const subtreeIDsMaxRows = 50000

// handleSubtreeIDs returns every file id+path under a given prefix,
// used by the lazy tree's folder-select checkbox so toggling a folder
// can select all its descendants without expanding them first.
func (s *Server) handleSubtreeIDs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	for len(prefix) > 0 && (prefix[0] == '/' || prefix[0] == '\\') {
		prefix = prefix[1:]
	}
	for len(prefix) > 0 && (prefix[len(prefix)-1] == '/' || prefix[len(prefix)-1] == '\\') {
		prefix = prefix[:len(prefix)-1]
	}
	if prefix == "" {
		writeError(w, http.StatusBadRequest, errors.New("prefix is required"))
		return
	}
	statusFilter := q.Get("status")
	key := "subtree-ids?" + url.Values{"prefix": {prefix}, "status": {statusFilter}}.Encode()
	database := s.deps.DB
	s.writeCachedFileJSON(w, r, database, key, func(ctx context.Context) (any, error) {
		ids, paths, total, err := database.ListSubtreeIDs(ctx, prefix, statusFilter, subtreeIDsMaxRows)
		if err != nil {
			return nil, err
		}
		return subtreeIDsResponse{
			IDs: ids, Paths: paths, Total: total,
			Truncated: total > int64(len(ids)),
		}, nil
	})
}

type fileStatsResponse struct {
	ByStatus            map[string]int64 `json:"by_status"`
	ByRestoreStatus     map[string]int64 `json:"by_restore_status"`
	RestoreSoonestExpAt *time.Time       `json:"restore_soonest_expires_at,omitempty"`
	TotalCount          int64            `json:"total_count"`
	TotalSize           int64            `json:"total_size"`
}

func (s *Server) handleFileStats(w http.ResponseWriter, r *http.Request) {
	database := s.deps.DB
	s.writeCachedFileJSON(w, r, database, "stats", func(ctx context.Context) (any, error) {
		st, err := database.Stats(ctx)
		if err != nil {
			return nil, err
		}
		return fileStatsResponse{
			ByStatus: st.ByStatus, ByRestoreStatus: st.ByRestoreStatus,
			RestoreSoonestExpAt: st.RestoreSoonestExp,
			TotalCount:          st.TotalCount, TotalSize: st.TotalSize,
		}, nil
	})
}

type responseStatusError struct {
	status int
	err    error
}

func (e *responseStatusError) Error() string      { return e.err.Error() }
func (e *responseStatusError) Unwrap() error      { return e.err }
func (e *responseStatusError) NonCacheable() bool { return true }

func (s *Server) writeCachedFileJSON(
	w http.ResponseWriter,
	r *http.Request,
	database *db.DB,
	key string,
	load func(context.Context) (any, error),
) {
	body, err := s.fileResponses.Get(r.Context(), key, database.FileRevision, func(ctx context.Context) ([]byte, error) {
		value, err := load(ctx)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(value); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	})
	if err != nil {
		var statusErr *responseStatusError
		if errors.As(err, &statusErr) {
			writeError(w, statusErr.status, statusErr.err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
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
