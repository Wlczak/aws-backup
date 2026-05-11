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
	var files []db.File
	var total int64
	var err error
	if all {
		files, total, err = s.cachedAllFiles(r.Context(), filter)
	} else {
		files, total, err = s.deps.DB.ListFiles(r.Context(), filter)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if all && total > maxAllRows {
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"index has %d rows, exceeds the %d-row cap on all=true; paginate via page/limit instead",
			total, maxAllRows,
		))
		return
	}
	out := make([]fileEntry, 0, len(files))
	for _, f := range files {
		out = append(out, fileEntry{
			ID: f.ID, Path: f.Path, Size: f.Size, MTime: f.MTime, MD5: f.MD5,
			Status: f.Status, ZipID: f.ZipID, ZipName: f.ZipName, S3Key: f.S3Key,
			UploadedAt: f.UploadedAt, LastSeenAt: f.LastSeenAt,
			RestoreStatus: f.RestoreStatus, RestoreExpiresAt: f.RestoreExpiresAt,
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

	folders, files, err := s.deps.DB.ListTreeChildren(r.Context(), prefix, statusFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	resp := treeListResponse{
		Prefix:  prefix,
		Folders: make([]treeFolderEntry, 0, len(folders)),
		Files:   make([]fileEntry, 0, len(files)),
	}
	for _, fd := range folders {
		resp.Folders = append(resp.Folders, treeFolderEntry{
			Name: fd.Name, Path: fd.Path,
			FileCount: fd.FileCount, TotalSize: fd.TotalSize,
		})
	}
	for _, f := range files {
		resp.Files = append(resp.Files, fileEntry{
			ID: f.ID, Path: f.Path, Size: f.Size, MTime: f.MTime, MD5: f.MD5,
			Status: f.Status, ZipID: f.ZipID, ZipName: f.ZipName, S3Key: f.S3Key,
			UploadedAt: f.UploadedAt, LastSeenAt: f.LastSeenAt,
			RestoreStatus: f.RestoreStatus, RestoreExpiresAt: f.RestoreExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, resp)
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
	ids, paths, total, err := s.deps.DB.ListSubtreeIDs(r.Context(), prefix, q.Get("status"), subtreeIDsMaxRows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, subtreeIDsResponse{
		IDs: ids, Paths: paths, Total: total,
		Truncated: total > int64(len(ids)),
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
	st, err := s.cachedStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, fileStatsResponse{
		ByStatus:            st.ByStatus,
		ByRestoreStatus:     st.ByRestoreStatus,
		RestoreSoonestExpAt: st.RestoreSoonestExp,
		TotalCount:          st.TotalCount,
		TotalSize:           st.TotalSize,
	})
}

// cachedStats serves /api/files/stats from a short TTL cache so tight
// dashboard polls don't rescan the whole files table per request.
//
// The hot read path (cache hit) only takes the mutex briefly; on miss
// we use singleflight so concurrent pollers share one DB call instead
// of queueing serially behind a held mutex (#179). On query error we
// set a short backoff expiry so a degraded DB isn't hammered.
func (s *Server) cachedStats(ctx context.Context) (db.FileStats, error) {
	s.statsMu.Lock()
	if time.Now().Before(s.statsExpiry) {
		v := s.statsValue
		e := s.statsErr
		s.statsMu.Unlock()
		return v, e
	}
	s.statsMu.Unlock()

	v, err, _ := s.statsSF.Do("stats", func() (any, error) {
		// Re-check the cache inside the singleflight winner: a concurrent
		// caller may have populated it while we were queueing.
		s.statsMu.Lock()
		if time.Now().Before(s.statsExpiry) {
			cached := s.statsValue
			cachedErr := s.statsErr
			s.statsMu.Unlock()
			if cachedErr != nil {
				return db.FileStats{}, cachedErr
			}
			return cached, nil
		}
		s.statsMu.Unlock()
		st, err := s.deps.DB.Stats(ctx)
		s.statsMu.Lock()
		defer s.statsMu.Unlock()
		if err != nil {
			// Stash the error on the cache for the backoff window so a
			// degraded DB doesn't replay instantly AND cache-hit callers
			// see the failure instead of a stale healthy value. (#243)
			s.statsValue = db.FileStats{}
			s.statsErr = err
			s.statsExpiry = time.Now().Add(500 * time.Millisecond)
			return db.FileStats{}, err
		}
		s.statsValue = st
		s.statsErr = nil
		s.statsExpiry = time.Now().Add(statsCacheTTL)
		return st, nil
	})
	if err != nil {
		return db.FileStats{}, err
	}
	return v.(db.FileStats), nil
}

// allFilesCacheTTL bounds staleness of the cached /api/files?all=true
// response. Mirrors statsCacheTTL — short enough that the tree view
// still reflects fresh state, long enough to absorb a 10–30 MB JSON
// hit on every poller. (#178)
const allFilesCacheTTL = 2 * time.Second

// cachedAllFiles serves the all=true path from a short TTL cache so a
// poll loop doesn't re-scan + re-serialise the full files table per
// request. Singleflight collapses concurrent misses into one query.
func (s *Server) cachedAllFiles(ctx context.Context, filter db.FilesFilter) ([]db.File, int64, error) {
	key := filter.Status + "|" + filter.Search
	now := time.Now()
	s.allFilesMu.Lock()
	if e, ok := s.allFilesCache[key]; ok && now.Before(e.expiry) {
		s.allFilesMu.Unlock()
		return e.files, e.total, e.err
	}
	s.allFilesMu.Unlock()

	v, err, _ := s.allFilesSF.Do("all|"+key, func() (any, error) {
		s.allFilesMu.Lock()
		if e, ok := s.allFilesCache[key]; ok && time.Now().Before(e.expiry) {
			s.allFilesMu.Unlock()
			return e, nil
		}
		s.allFilesMu.Unlock()
		files, total, qerr := s.deps.DB.ListFiles(ctx, filter)
		s.allFilesMu.Lock()
		defer s.allFilesMu.Unlock()
		if qerr != nil {
			// Cache the error itself for a short backoff window so a
			// degraded DB isn't replayed per poll, but cache-hit callers
			// keep seeing the failure instead of an empty 200. (#271)
			entry := allFilesCacheEntry{expiry: time.Now().Add(500 * time.Millisecond), err: qerr}
			s.allFilesCache[key] = entry
			return entry, qerr
		}
		entry := allFilesCacheEntry{files: files, total: total, expiry: time.Now().Add(allFilesCacheTTL)}
		s.allFilesCache[key] = entry
		return entry, nil
	})
	if err != nil {
		return nil, 0, err
	}
	e := v.(allFilesCacheEntry)
	return e.files, e.total, e.err
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
