package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/pathutil"
	"github.com/Wlczak/aws-backup/internal/storage"
)

type syncResponse struct {
	ZipNamesInDB       int   `json:"zip_names_in_db"`
	IndividualKeysInDB int   `json:"individual_keys_in_db"`
	KeysInS3           int   `json:"keys_in_s3"`
	MissingZips        int   `json:"missing_zips"`
	MissingIndividual  int   `json:"missing_individual"`
	FilesCreated       int64 `json:"files_created"`
	FilesReset         int64 `json:"files_reset"`
}

// fullSyncResponse extends syncResponse with a content-level diff: every
// local file is checked against the union of zip indexes + standalone keys
// in S3, so the caller sees which local files still need backing up and
// which cloud-only files exist (candidates for restore).
type fullSyncResponse struct {
	syncResponse

	CloudFileCount        int      `json:"cloud_file_count"`
	LocalFileCount        int      `json:"local_file_count"`
	ZipIndexesConsumed    int      `json:"zip_indexes_consumed"`
	LocalMissingCount     int      `json:"local_missing_count"`
	LocalMissingFromCloud []string `json:"local_missing_from_cloud,omitempty"`
	CloudMissingCount     int      `json:"cloud_missing_count"`
	CloudMissingFromLocal []string `json:"cloud_missing_from_local,omitempty"`
}

// fullSyncListCap bounds how many paths we return in each missing-from-X
// list. The full counts are always reported; the lists themselves are just
// for a preview in the UI so we don't send megabytes of JSON on big
// backups.
const fullSyncListCap = 1000

// handleSync runs the authoritative cloud compare: list every object in the
// bucket, download every zip index, compare the resulting path set to the
// locally scanned rows, recreate any cloud-only paths that are missing from
// the local index, and reset only rows that are still local but whose
// recorded object is no longer present.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	st := s.storage()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("storage not configured"))
		return
	}
	// Gate on engine-idle: while a run is in flight the engine writes
	// status / s3_key / zip_name on the same rows MarkPending* flips. A
	// stale List page could observe a just-uploaded key as missing and
	// reset it to pending. (#222)
	s.runMu.Lock()
	busy := s.currentRun != 0
	s.runMu.Unlock()
	if busy {
		writeError(w, http.StatusConflict,
			errors.New("a backup run is in progress — sync would race engine writes; try again when idle"))
		return
	}
	// Detach the work ctx from the request: listing the bucket plus
	// reconciling the DB over a multi-million-key bucket can outlive a
	// tab close, and a client disconnect that fires after List completes
	// but mid-reset would leave a partial update. Server-controlled
	// deadline bounds it. (#238)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
	defer cancel()
	resp, err := s.runSyncCloudCompare(ctx, st)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSyncFull reports the same authoritative cloud compare as handleSync.
// The route remains as a compatibility alias for callers that still post to
// /api/sync/full.
func (s *Server) handleSyncFull(w http.ResponseWriter, r *http.Request) {
	st := s.storage()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("storage not configured"))
		return
	}
	// Engine-idle gate — see handleSync. (#222)
	s.runMu.Lock()
	busy := s.currentRun != 0
	s.runMu.Unlock()
	if busy {
		writeError(w, http.StatusConflict,
			errors.New("a backup run is in progress — full sync would race engine writes; try again when idle"))
		return
	}
	// Detach from request ctx — see handleSync. (#238)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()
	resp, err := s.runSyncCloudCompare(ctx, st)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// runSyncCloudCompare runs the authoritative sync pass: list the bucket,
// download all zip indexes, compare the resulting cloud path set to the local
// rows that are still on disk, recreate missing rows for cloud-only objects as
// cloud_only, and promote any S3-present local rows into either uploaded or
// cloud_only so the bucket-backed state is always explicit.
func (s *Server) runSyncCloudCompare(ctx context.Context, st storage.Storage) (fullSyncResponse, error) {
	zipNames, err := s.deps.DB.ListZipNames(ctx)
	if err != nil {
		return fullSyncResponse{}, fmt.Errorf("list zip names: %w", err)
	}

	indivKeys, err := s.deps.DB.ListIndividualS3Keys(ctx)
	if err != nil {
		return fullSyncResponse{}, fmt.Errorf("list individual keys: %w", err)
	}

	prefix := s.storagePrefix()
	s3Keys, err := st.List(ctx, pathutil.NormalizeS3ListPrefix(prefix))
	if err != nil {
		return fullSyncResponse{}, fmt.Errorf("list s3 keys: %w", err)
	}
	inS3 := make(map[string]struct{}, len(s3Keys))
	for _, k := range s3Keys {
		inS3[k] = struct{}{}
	}

	idx, err := engine.LoadCloudIndex(ctx, st, prefix, indivKeys)
	if err != nil {
		return fullSyncResponse{}, fmt.Errorf("load cloud index: %w", err)
	}

	existingPaths := map[string]struct{}{}
	localPaths := map[string]struct{}{}
	promoteUploaded := make([]db.FileUpdate, 0)
	recreateRows := make([]db.File, 0)
	now := time.Now().UTC()
	const pageSize = 1000
	zipHeadCache := map[string]storage.HeadResult{}
	getHead := func(key string) (storage.HeadResult, bool) {
		if head, ok := zipHeadCache[key]; ok {
			return head, true
		}
		head, err := st.Head(ctx, key)
		if err != nil {
			return storage.HeadResult{}, false
		}
		zipHeadCache[key] = head
		return head, true
	}
	buildFields := func(cf engine.CloudFile, status string) map[string]any {
		fields := map[string]any{
			"status": status,
		}
		s3Key := cf.S3Key
		if cf.ZipKey != "" {
			s3Key = cf.ZipKey
			fields["zip_name"] = strings.TrimPrefix(cf.ZipKey, prefix)
		} else {
			fields["zip_name"] = ""
		}
		fields["s3_key"] = s3Key
		if head, ok := getHead(s3Key); ok {
			fields["uploaded_at"] = head.LastModified.UTC()
			if status == db.StatusCloudOnly {
				fields["mtime"] = head.LastModified.UTC()
				if cf.ZipKey == "" {
					fields["size"] = head.Size
				}
			}
		} else {
			fields["uploaded_at"] = now
			if status == db.StatusCloudOnly {
				fields["mtime"] = now
			}
		}
		return fields
	}
	for p := 1; ; p++ {
		if err := ctx.Err(); err != nil {
			return fullSyncResponse{}, err
		}
		rows, _, err := s.deps.DB.ListFiles(ctx, db.FilesFilter{Page: p, Limit: pageSize})
		if err != nil {
			return fullSyncResponse{}, fmt.Errorf("list files: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, f := range rows {
			existingPaths[f.Path] = struct{}{}
			// Missing/cloud-only rows are already known to be absent from the
			// source, so they stay out of the local diff. cloud_only rows remain
			// recoverable only while their S3 object still exists.
			if f.Status == db.StatusMissing {
				if cf, ok := idx.Files[f.Path]; ok {
					promoteUploaded = append(promoteUploaded, db.FileUpdate{
						ID:     f.ID,
						Fields: buildFields(cf, db.StatusCloudOnly),
					})
				}
				continue
			}
			if cf, ok := idx.Files[f.Path]; ok {
				if f.Status != db.StatusUploaded {
					promoteUploaded = append(promoteUploaded, db.FileUpdate{
						ID:     f.ID,
						Fields: buildFields(cf, db.StatusUploaded),
					})
				}
				continue
			}
			localPaths[f.Path] = struct{}{}
			if _, ok := idx.Files[f.Path]; ok {
				continue
			}
			if f.Status != db.StatusPending || f.MD5 != "" || f.ZipName != "" || f.S3Key != "" || !f.UploadedAt.IsZero() {
				promoteUploaded = append(promoteUploaded, db.FileUpdate{
					ID: f.ID,
					Fields: map[string]any{
						"status":      db.StatusPending,
						"md5":         nil,
						"zip_name":    nil,
						"s3_key":      nil,
						"uploaded_at": nil,
					},
				})
			}
		}
		if len(rows) < pageSize {
			break
		}
	}

	cloudPaths := make([]string, 0, len(idx.Files))
	for p := range idx.Files {
		if _, ok := existingPaths[p]; ok {
			continue
		}
		cloudPaths = append(cloudPaths, p)
	}
	sort.Strings(cloudPaths)
	for _, p := range cloudPaths {
		cf := idx.Files[p]
		row := db.File{
			Path:       p,
			Size:       0,
			MTime:      time.Unix(0, 0).UTC(),
			Status:     db.StatusCloudOnly,
			LastSeenAt: now,
		}
		if cf.ZipKey != "" {
			row.ZipName = strings.TrimPrefix(cf.ZipKey, prefix)
			row.S3Key = cf.ZipKey
			head, ok := zipHeadCache[cf.ZipKey]
			if !ok {
				if h, err := st.Head(ctx, cf.ZipKey); err == nil {
					head = h
					zipHeadCache[cf.ZipKey] = h
					ok = true
				}
			}
			if ok {
				row.MTime = head.LastModified.UTC()
				row.UploadedAt = head.LastModified.UTC()
			} else {
				row.MTime = now
				row.UploadedAt = now
			}
		} else {
			row.S3Key = cf.S3Key
			if head, err := st.Head(ctx, cf.S3Key); err == nil {
				row.Size = head.Size
				row.MTime = head.LastModified.UTC()
				row.UploadedAt = head.LastModified.UTC()
			} else {
				row.MTime = now
				row.UploadedAt = now
			}
		}
		recreateRows = append(recreateRows, row)
	}

	var filesCreated int64
	if len(recreateRows) > 0 {
		n, err := s.deps.DB.CreateFiles(ctx, recreateRows)
		if err != nil {
			return fullSyncResponse{}, fmt.Errorf("recreate cloud-only files: %w", err)
		}
		filesCreated = n
	}

	var filesReset int64
	if len(promoteUploaded) > 0 {
		if err := s.deps.DB.UpdateFiles(ctx, promoteUploaded); err != nil {
			return fullSyncResponse{}, fmt.Errorf("promote uploaded files: %w", err)
		}
	}
	if len(promoteUploaded) > 0 {
		filesReset = int64(len(promoteUploaded))
	}

	resp := fullSyncResponse{
		syncResponse: syncResponse{
			ZipNamesInDB:       len(zipNames),
			IndividualKeysInDB: len(indivKeys),
			KeysInS3:           len(s3Keys),
			FilesCreated:       filesCreated,
			FilesReset:         filesReset,
		},
		CloudFileCount:     len(idx.Files),
		LocalFileCount:     len(localPaths),
		ZipIndexesConsumed: idx.IndexCount,
	}

	for p := range localPaths {
		if _, ok := idx.Files[p]; !ok {
			resp.LocalMissingCount++
			if len(resp.LocalMissingFromCloud) < fullSyncListCap {
				resp.LocalMissingFromCloud = append(resp.LocalMissingFromCloud, p)
			}
		}
	}
	for p := range idx.Files {
		if _, ok := localPaths[p]; !ok {
			resp.CloudMissingCount++
			if len(resp.CloudMissingFromLocal) < fullSyncListCap {
				resp.CloudMissingFromLocal = append(resp.CloudMissingFromLocal, p)
			}
		}
	}

	resp.MissingZips = 0
	resp.MissingIndividual = 0
	for _, z := range zipNames {
		key := pathutil.JoinKey(prefix, z)
		if _, ok := inS3[key]; ok {
			continue
		}
		resp.MissingZips++
	}
	for _, k := range indivKeys {
		if _, ok := inS3[k]; !ok {
			resp.MissingIndividual++
		}
	}

	return resp, nil
}

type deleteCloudPathsRequest struct {
	Paths []string `json:"paths"`
}

type deleteCloudPathsResponse struct {
	DeletedStandalone int      `json:"deleted_standalone"`
	DeletedZips       int      `json:"deleted_zips"`
	SkippedPartialZip int      `json:"skipped_partial_zip"`
	Errors            []string `json:"errors,omitempty"`
}

// handleDeleteCloudPaths removes cloud-only objects identified by source-relative
// paths. Standalone S3 objects are deleted immediately. Zip archives are deleted
// only when every file they contain is listed in the request (safe whole-zip
// removal); zips that contain a mix of targeted and non-targeted files are
// skipped and reported in skipped_partial_zip.
func (s *Server) handleDeleteCloudPaths(w http.ResponseWriter, r *http.Request) {
	st := s.storage()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("storage not configured"))
		return
	}
	var req deleteCloudPathsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("paths must be non-empty"))
		return
	}

	// Engine-idle gate — MarkMissingByPaths writes the same s3_key rows
	// the engine touches mid-run. (#222)
	s.runMu.Lock()
	busy := s.currentRun != 0
	s.runMu.Unlock()
	if busy {
		writeError(w, http.StatusConflict,
			errors.New("a backup run is in progress — delete-cloud-paths would race engine writes; try again when idle"))
		return
	}

	// Detach from request ctx so a tab close mid-delete doesn't leave the
	// DB out of sync with the bucket — the cloud-side Delete may have
	// landed but MarkMissingByPaths needs to follow. (#238)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()

	indivKeys, err := s.deps.DB.ListIndividualS3Keys(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("list individual keys: %w", err))
		return
	}
	prefix := s.storagePrefix()
	idx, err := engine.LoadCloudIndex(ctx, st, prefix, indivKeys)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("load cloud index: %w", err))
		return
	}

	targeted := make(map[string]struct{}, len(req.Paths))
	for _, p := range req.Paths {
		targeted[p] = struct{}{}
	}

	// Build a reverse map: zipKey → all paths in that zip (from the full index).
	zipContents := map[string][]string{}
	for path, cf := range idx.Files {
		if cf.ZipKey != "" {
			zipContents[cf.ZipKey] = append(zipContents[cf.ZipKey], path)
		}
	}

	resp := deleteCloudPathsResponse{}

	// Track which source-paths actually had their backing object deleted
	// so we can flip the matching DB rows to `missing` afterwards. Without
	// this the next existence check observes the absence and re-queues
	// the file for upload (#132). Per the project index-semantics rule we
	// mark missing rather than delete, so the row continues to track
	// bucket state.
	var markMissing []string

	// Delete standalone objects.
	for _, p := range req.Paths {
		cf, ok := idx.Files[p]
		if !ok || cf.S3Key == "" {
			continue
		}
		if err := st.Delete(ctx, cf.S3Key); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("delete %s: %v", cf.S3Key, err))
		} else {
			resp.DeletedStandalone++
			markMissing = append(markMissing, p)
		}
	}

	// Determine which zips can be safely deleted in full.
	// A zip is safe to delete only when every path it contains is targeted.
	deletedZips := map[string]struct{}{}
	for _, p := range req.Paths {
		cf, ok := idx.Files[p]
		if !ok || cf.ZipKey == "" {
			continue
		}
		if _, done := deletedZips[cf.ZipKey]; done {
			continue
		}
		allTargeted := true
		for _, zipPath := range zipContents[cf.ZipKey] {
			if _, ok := targeted[zipPath]; !ok {
				allTargeted = false
				break
			}
		}
		if !allTargeted {
			resp.SkippedPartialZip++
			continue
		}
		// Delete the zip and its .index.txt sidecar.
		zipS3Key := pathutil.JoinKey(prefix, cf.ZipKey)
		if err := st.Delete(ctx, zipS3Key); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("delete zip %s: %v", zipS3Key, err))
			continue
		}
		_ = st.Delete(ctx, zipS3Key+engine.ZipIndexSuffix) // best-effort
		deletedZips[cf.ZipKey] = struct{}{}
		resp.DeletedZips++
		// Every path inside a successfully-deleted whole-zip is now
		// missing from the bucket; flip them in one batch below. (#132)
		markMissing = append(markMissing, zipContents[cf.ZipKey]...)
	}

	if len(markMissing) > 0 {
		if _, err := s.deps.DB.MarkMissingByPaths(ctx, markMissing); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("mark missing: %v", err))
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
