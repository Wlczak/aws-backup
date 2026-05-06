package api

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/pathutil"
	"github.com/Wlczak/aws-backup/internal/restore/inventory"
	"github.com/Wlczak/aws-backup/internal/restore/scanner"
)

// Deep Archive pricing the estimator uses. Kept in code (not config) so
// the UI shows the same numbers everyone else sees; real billing still
// comes from AWS.
const (
	pricePerThousandRequests = 0.10  // USD; GET requests for restored objects
	pricePerGBRetrieval      = 0.02  // USD/GB; Deep Archive standard retrieval
	pricePerGBEgress         = 0.09  // USD/GB; internet egress after free tier
	egressFreeGB             = 100.0 // free tier
	retrievalHoursMin        = 12
	retrievalHoursMax        = 48
)

type restoreEstimateRequest struct {
	Paths []string `json:"paths"`
}

type restoreEstimateResponse struct {
	FileCount       int64    `json:"file_count"`
	TotalBytes      int64    `json:"total_bytes"`
	RequestFeeUSD   float64  `json:"request_fee_usd"`
	RetrievalFeeUSD float64  `json:"retrieval_fee_usd"`
	EgressFeeUSD    float64  `json:"egress_fee_usd"`
	TotalFeeUSD     float64  `json:"total_fee_usd"`
	WaitHoursMin    int      `json:"wait_hours_min"`
	WaitHoursMax    int      `json:"wait_hours_max"`
	UnknownPaths    []string `json:"unknown_paths,omitempty"`
}

// handleRestoreEstimate computes a cost breakdown from DB metadata only —
// it does not talk to S3 / AWS.
func (s *Server) handleRestoreEstimate(w http.ResponseWriter, r *http.Request) {
	var req restoreEstimateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("paths must be non-empty"))
		return
	}

	// "/" (or "") is a special sentinel meaning "all files".
	allFiles := false
	for _, p := range req.Paths {
		if p == "/" || p == "" {
			allFiles = true
			break
		}
	}

	want := make(map[string]struct{}, len(req.Paths))
	for _, p := range req.Paths {
		want[p] = struct{}{}
	}

	var (
		count   int64
		bytes   int64
		unknown []string
	)

	// List files in pages; match by path prefix (so a "photos" entry
	// catches "photos/*").
	const pageSize = 1000
	matched := map[string]bool{}
	for p := 1; ; p++ {
		rows, _, err := s.deps.DB.ListFiles(r.Context(), db.FilesFilter{Page: p, Limit: pageSize})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if len(rows) == 0 {
			break
		}
		for _, f := range rows {
			// Only files actually in S3 are restorable. pending/failed/missing
			// rows aren't backed up yet (or any more), so counting them inflates
			// the dollar estimate. Mirrors the skip in engine.RestoreToDir.
			if f.Status != db.StatusUploaded && f.Status != db.StatusZipped {
				continue
			}
			if allFiles {
				count++
				bytes += f.Size
			} else {
				for wantPath := range want {
					if pathutil.HasPrefixPath(f.Path, wantPath) {
						count++
						bytes += f.Size
						matched[wantPath] = true
						break
					}
				}
			}
		}
		if len(rows) < pageSize {
			break
		}
	}
	if !allFiles {
		for wantPath := range want {
			if !matched[wantPath] {
				unknown = append(unknown, wantPath)
			}
		}
	}

	gb := float64(bytes) / (1024 * 1024 * 1024)
	request := float64(count) * pricePerThousandRequests / 1000
	retrieval := gb * pricePerGBRetrieval
	egressGB := gb
	if egressGB > egressFreeGB {
		egressGB -= egressFreeGB
	} else {
		egressGB = 0
	}
	egress := egressGB * pricePerGBEgress

	writeJSON(w, http.StatusOK, restoreEstimateResponse{
		FileCount:       count,
		TotalBytes:      bytes,
		RequestFeeUSD:   round2(request),
		RetrievalFeeUSD: round2(retrieval),
		EgressFeeUSD:    round2(egress),
		TotalFeeUSD:     round2(request + retrieval + egress),
		WaitHoursMin:    retrievalHoursMin,
		WaitHoursMax:    retrievalHoursMax,
		UnknownPaths:    unknown,
	})
}

type restoreTriggerRequest struct {
	Paths     []string `json:"paths"`
	TargetDir string   `json:"target_dir"`
}

type restoreTriggerResponse struct {
	FilesWritten int64    `json:"files_written"`
	BytesWritten int64    `json:"bytes_written"`
	Skipped      []string `json:"skipped,omitempty"`
	Errors       []string `json:"errors,omitempty"`
}

// handleRestoreTrigger downloads each selected file from S3 and writes
// it under target_dir. Files stored individually are streamed directly
// from their s3_key; files stored inside a zip archive are downloaded
// once per archive and extracted selectively.
//
// This path assumes the S3 objects are already retrievable. If the
// bucket stores them in DEEP_ARCHIVE and they haven't been restored out
// of band, Storage.Get will fail and the per-file error is reported in
// the response. Wiring s3:RestoreObject + readiness polling is a
// follow-up (see the "feature 19" note).
func (s *Server) handleRestoreTrigger(w http.ResponseWriter, r *http.Request) {
	st := s.storage()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("storage not configured"))
		return
	}

	var req restoreTriggerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("paths must be non-empty"))
		return
	}
	if req.TargetDir == "" {
		writeError(w, http.StatusBadRequest, errors.New("target_dir is required"))
		return
	}
	if !filepath.IsAbs(req.TargetDir) {
		writeError(w, http.StatusBadRequest, errors.New("target_dir must be an absolute path"))
		return
	}

	var tmpDir string
	if cfg, ok := s.snapshotConfig(); ok {
		tmpDir = cfg.Backup.TmpDir
	}
	stats, err := engine.RestoreToDir(r.Context(), engine.RestoreOptions{
		DB:        s.deps.DB,
		Storage:   st,
		KeyPrefix: s.storagePrefix(),
		TargetDir: req.TargetDir,
		Paths:     req.Paths,
		TmpDir:    tmpDir,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, restoreTriggerResponse{
		FilesWritten: stats.FilesWritten,
		BytesWritten: stats.BytesWritten,
		Skipped:      stats.Skipped,
		Errors:       stats.Errors,
	})
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

// handleRestoreSyncStatus drains the configured SQS queue of pending S3
// Glacier restore events and applies them to the DB. Returns the number
// of messages processed. 503 when SQS isn't configured so the UI can
// surface that as a friendly "no queue configured" message.
func (s *Server) handleRestoreSyncStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.SyncRestoreStatus == nil {
		writeError(w, http.StatusServiceUnavailable,
			fmt.Errorf("SQS restore consumer is not configured (set sqs.queue_url in config)"))
		return
	}
	processed, err := s.deps.SyncRestoreStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("sync restore status: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"processed": processed})
}

// handleRestoreScanFull triggers an authoritative full HEAD-per-object
// reconciliation of restore status. Long-running on big indexes;
// progress streams via SSE (restore_scan_*).
func (s *Server) handleRestoreScanFull(w http.ResponseWriter, r *http.Request) {
	if s.deps.RestoreScanner == nil {
		writeError(w, http.StatusServiceUnavailable,
			errors.New("restore scanner not configured (storage missing)"))
		return
	}
	res, err := s.deps.RestoreScanner.RunFull(r.Context())
	if err != nil {
		if errors.Is(err, scanner.ErrBusy) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Errorf("restore scan full: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleRestoreScanPending HEADs only files locally marked in_progress
// to catch SQS notifications that never arrived.
func (s *Server) handleRestoreScanPending(w http.ResponseWriter, r *http.Request) {
	if s.deps.RestoreScanner == nil {
		writeError(w, http.StatusServiceUnavailable,
			errors.New("restore scanner not configured (storage missing)"))
		return
	}
	res, err := s.deps.RestoreScanner.RunPending(r.Context())
	if err != nil {
		if errors.Is(err, scanner.ErrBusy) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Errorf("restore scan pending: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleInventoryGet returns the current S3 inventory configuration on
// the backup bucket, or {enabled: false} when none is set.
func (s *Server) handleInventoryGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.Inventory == nil {
		writeError(w, http.StatusServiceUnavailable,
			errors.New("inventory manager not configured (storage missing)"))
		return
	}
	st, err := s.deps.Inventory.Get(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("get inventory: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type inventoryPutRequest struct {
	Frequency string `json:"frequency"` // "daily" | "weekly"
}

// handleInventoryPut installs (or replaces) the bucket's inventory
// configuration with the requested frequency.
func (s *Server) handleInventoryPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Inventory == nil {
		writeError(w, http.StatusServiceUnavailable,
			errors.New("inventory manager not configured (storage missing)"))
		return
	}
	var req inventoryPutRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	freq, err := inventory.ParseFrequency(req.Frequency)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.deps.Inventory.Put(r.Context(), freq); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("put inventory: %w", err))
		return
	}
	st, err := s.deps.Inventory.Get(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("read back inventory: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleInventoryDelete removes the inventory configuration. Idempotent.
func (s *Server) handleInventoryDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.Inventory == nil {
		writeError(w, http.StatusServiceUnavailable,
			errors.New("inventory manager not configured (storage missing)"))
		return
	}
	if err := s.deps.Inventory.Delete(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("delete inventory: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
}

// handleInventorySync enumerates keys from the latest inventory manifest
// and runs them through the restore scanner.
func (s *Server) handleInventorySync(w http.ResponseWriter, r *http.Request) {
	if s.deps.Inventory == nil || s.deps.RestoreScanner == nil {
		writeError(w, http.StatusServiceUnavailable,
			errors.New("inventory manager or scanner not configured (storage missing)"))
		return
	}
	keys, err := s.deps.Inventory.ListLatestKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("list inventory keys: %w", err))
		return
	}
	res, err := s.deps.RestoreScanner.RunKeys(r.Context(), scanner.ModeInventory, keys)
	if err != nil {
		if errors.Is(err, scanner.ErrBusy) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Errorf("inventory scan: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, res)
}
