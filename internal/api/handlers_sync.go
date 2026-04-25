package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/pathutil"
)

type syncResponse struct {
	ZipNamesInDB       int   `json:"zip_names_in_db"`
	IndividualKeysInDB int   `json:"individual_keys_in_db"`
	KeysInS3           int   `json:"keys_in_s3"`
	MissingZips        int   `json:"missing_zips"`
	MissingIndividual  int   `json:"missing_individual"`
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

// handleSync checks that every recorded S3 object still exists in the bucket.
// Zipped files are matched by zip_name; individually-uploaded files by s3_key.
// Missing objects are reset to pending so the next run re-uploads them.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if s.deps.Storage == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("storage not configured"))
		return
	}
	resp, err := s.runSyncExistenceCheck(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}


// handleSyncFull reports whether every local file is covered in the cloud
// (zip indexes + standalone keys) and vice versa. This is heavier than
// handleSync — it downloads every .index.txt sidecar — so it's exposed on
// a separate route rather than folded into the default sync.
func (s *Server) handleSyncFull(w http.ResponseWriter, r *http.Request) {
	if s.deps.Storage == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("storage not configured"))
		return
	}
	ctx := r.Context()

	// Start from the same existence check handleSync does so callers get
	// both views in one call; reuse helpers rather than duplicating logic.
	base, err := s.runSyncExistenceCheck(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	indivKeys, err := s.deps.DB.ListIndividualS3Keys(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("list individual keys: %w", err))
		return
	}

	idx, err := engine.LoadCloudIndex(ctx, s.deps.Storage, s.deps.StoragePrefix, indivKeys)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("load cloud index: %w", err))
		return
	}

	// Walk every file currently in the DB. Files that were scanned but
	// haven't uploaded yet (status=pending/failed) still count as "local"
	// — the user wants to know they exist on disk but not in the cloud.
	localPaths := map[string]struct{}{}
	const pageSize = 1000
	for p := 1; ; p++ {
		rows, _, err := s.deps.DB.ListFiles(ctx, db.FilesFilter{Page: p, Limit: pageSize})
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("list files: %w", err))
			return
		}
		if len(rows) == 0 {
			break
		}
		for _, f := range rows {
			// A file marked "missing" exists in the DB but not on disk —
			// for sync purposes it isn't local, so skip it.
			if f.Status == db.StatusMissing {
				continue
			}
			localPaths[f.Path] = struct{}{}
		}
		if len(rows) < pageSize {
			break
		}
	}

	resp := fullSyncResponse{
		syncResponse:       base,
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

	writeJSON(w, http.StatusOK, resp)
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
	if s.deps.Storage == nil {
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

	ctx := r.Context()

	indivKeys, err := s.deps.DB.ListIndividualS3Keys(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("list individual keys: %w", err))
		return
	}
	idx, err := engine.LoadCloudIndex(ctx, s.deps.Storage, s.deps.StoragePrefix, indivKeys)
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

	// Delete standalone objects.
	for _, p := range req.Paths {
		cf, ok := idx.Files[p]
		if !ok || cf.S3Key == "" {
			continue
		}
		if err := s.deps.Storage.Delete(ctx, cf.S3Key); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("delete %s: %v", cf.S3Key, err))
		} else {
			resp.DeletedStandalone++
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
		zipS3Key := pathutil.JoinKey(s.deps.StoragePrefix, cf.ZipKey)
		if err := s.deps.Storage.Delete(ctx, zipS3Key); err != nil {
			resp.Errors = append(resp.Errors, fmt.Sprintf("delete zip %s: %v", zipS3Key, err))
			continue
		}
		_ = s.deps.Storage.Delete(ctx, zipS3Key+engine.ZipIndexSuffix) // best-effort
		deletedZips[cf.ZipKey] = struct{}{}
		resp.DeletedZips++
	}

	writeJSON(w, http.StatusOK, resp)
}

// runSyncExistenceCheck runs the cheap existence-only sync (used by both
// POST /api/sync and POST /api/sync/full) and resets any files whose S3
// objects have gone missing. Returns the summary fields without writing
// an HTTP response so callers can embed it in a larger payload.
func (s *Server) runSyncExistenceCheck(ctx context.Context) (syncResponse, error) {
	zipNames, err := s.deps.DB.ListZipNames(ctx)
	if err != nil {
		return syncResponse{}, fmt.Errorf("list zip names: %w", err)
	}

	indivKeys, err := s.deps.DB.ListIndividualS3Keys(ctx)
	if err != nil {
		return syncResponse{}, fmt.Errorf("list individual keys: %w", err)
	}

	s3Keys, err := s.deps.Storage.List(ctx, s.deps.StoragePrefix)
	if err != nil {
		return syncResponse{}, fmt.Errorf("list s3 keys: %w", err)
	}

	inS3 := make(map[string]struct{}, len(s3Keys))
	for _, k := range s3Keys {
		inS3[k] = struct{}{}
	}

	var missingZips []string
	for _, z := range zipNames {
		key := pathutil.JoinKey(s.deps.StoragePrefix, z)
		if _, ok := inS3[key]; !ok {
			missingZips = append(missingZips, z)
		}
	}
	var missingIndiv []string
	for _, k := range indivKeys {
		if _, ok := inS3[k]; !ok {
			missingIndiv = append(missingIndiv, k)
		}
	}

	var reset int64
	if len(missingZips) > 0 {
		n, err := s.deps.DB.MarkPendingByZipNames(ctx, missingZips)
		if err != nil {
			return syncResponse{}, fmt.Errorf("reset missing zips: %w", err)
		}
		reset += n
	}
	if len(missingIndiv) > 0 {
		n, err := s.deps.DB.MarkPendingByS3Keys(ctx, missingIndiv)
		if err != nil {
			return syncResponse{}, fmt.Errorf("reset missing individual: %w", err)
		}
		reset += n
	}

	return syncResponse{
		ZipNamesInDB:       len(zipNames),
		IndividualKeysInDB: len(indivKeys),
		KeysInS3:           len(s3Keys),
		MissingZips:        len(missingZips),
		MissingIndividual:  len(missingIndiv),
		FilesReset:         reset,
	}, nil
}
