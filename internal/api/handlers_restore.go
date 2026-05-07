package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/pathutil"
	"github.com/Wlczak/aws-backup/internal/restore/inventory"
	"github.com/Wlczak/aws-backup/internal/restore/scanner"
	"github.com/Wlczak/aws-backup/internal/storage"
)

// Deep Archive pricing the estimator uses. Kept in code (not config) so
// the UI shows the same numbers everyone else sees; real billing still
// comes from AWS.
const (
	pricePerThousandRequestsStandard = 0.11  // USD; Glacier standard retrieval requests
	pricePerGBRetrievalStandard      = 0.02  // USD/GB; Glacier standard retrieval
	pricePerThousandRequestsBulk     = 0.025 // USD; Glacier bulk retrieval requests
	pricePerGBRetrievalBulk          = 0.003 // USD/GB; Glacier bulk retrieval
	pricePerGBEgress                 = 0.09  // USD/GB; internet egress after free tier
	egressFreeGB                     = 100.0 // free tier
)

type restoreTierRequest string

const (
	restoreTierStandard restoreTierRequest = "standard"
	restoreTierBulk     restoreTierRequest = "bulk"
)

func parseRestoreTier(raw string) (storage.RestoreTier, error) {
	switch raw {
	case string(restoreTierStandard):
		return storage.RestoreTierStandard, nil
	case string(restoreTierBulk):
		return storage.RestoreTierBulk, nil
	default:
		return "", fmt.Errorf("tier must be either %q or %q", restoreTierBulk, restoreTierStandard)
	}
}

func restoreTierPricing(tier storage.RestoreTier) (requestPerThousand, retrievalPerGB float64, waitHoursMin, waitHoursMax int) {
	switch tier {
	case storage.RestoreTierBulk:
		return pricePerThousandRequestsBulk, pricePerGBRetrievalBulk, 48, 48
	default:
		return pricePerThousandRequestsStandard, pricePerGBRetrievalStandard, 12, 12
	}
}

type restoreEstimateRequest struct {
	Paths []string `json:"paths"`
	Tier  string   `json:"tier"`
}

type restoreEstimateResponse struct {
	// FileCount / TotalBytes count only the actual S3 objects that will
	// be retrieved — i.e. one zip archive per zip group, plus standalone
	// objects. Rows already in_progress or restored are excluded. The
	// request fee is computed off FileCount; TotalBytes remains the
	// current DB-byte estimate for the matching rows.
	FileCount       int64   `json:"file_count"`
	TotalBytes      int64   `json:"total_bytes"`
	RequestFeeUSD   float64 `json:"request_fee_usd"`
	RetrievalFeeUSD float64 `json:"retrieval_fee_usd"`
	EgressFeeUSD    float64 `json:"egress_fee_usd"`
	TotalFeeUSD     float64 `json:"total_fee_usd"`
	WaitHoursMin    int     `json:"wait_hours_min"`
	WaitHoursMax    int     `json:"wait_hours_max"`
	// Already* fields surface files that would otherwise have been
	// included in the trigger but are filtered out because S3 already
	// has (or is producing) a thawed copy. Shown in the UI so the
	// operator understands why FileCount may be smaller than expected.
	AlreadyInProgressCount int64    `json:"already_in_progress_count"`
	AlreadyInProgressBytes int64    `json:"already_in_progress_bytes"`
	AlreadyRestoredCount   int64    `json:"already_restored_count"`
	AlreadyRestoredBytes   int64    `json:"already_restored_bytes"`
	UnknownPaths           []string `json:"unknown_paths,omitempty"`
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
	tier, err := parseRestoreTier(req.Tier)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// "/" (or "") is a special sentinel meaning "all files".
	allFiles := false
	pathsForFilter := make([]string, 0, len(req.Paths))
	seen := make(map[string]struct{}, len(req.Paths))
	for _, p := range req.Paths {
		if p == "/" || p == "" {
			allFiles = true
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		pathsForFilter = append(pathsForFilter, p)
	}

	br, err := s.deps.DB.RestoreEstimateStats(r.Context(), pathsForFilter, allFiles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	objectCount, err := s.restoreObjectCount(r.Context(), pathsForFilter, allFiles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	var unknown []string
	if !allFiles {
		matchedSet := make(map[string]struct{}, len(br.MatchedPaths))
		for _, p := range br.MatchedPaths {
			matchedSet[p] = struct{}{}
		}
		for _, p := range pathsForFilter {
			if _, ok := matchedSet[p]; !ok {
				unknown = append(unknown, p)
			}
		}
	}

	requestPerThousand, retrievalPerGB, waitHoursMin, waitHoursMax := restoreTierPricing(tier)
	gb := float64(br.RetrievableBytes) / (1024 * 1024 * 1024)
	request := float64(objectCount) * requestPerThousand / 1000
	retrieval := gb * retrievalPerGB
	egressGB := gb
	if egressGB > egressFreeGB {
		egressGB -= egressFreeGB
	} else {
		egressGB = 0
	}
	egress := egressGB * pricePerGBEgress

	writeJSON(w, http.StatusOK, restoreEstimateResponse{
		FileCount:              objectCount,
		TotalBytes:             br.RetrievableBytes,
		RequestFeeUSD:          round2(request),
		RetrievalFeeUSD:        round2(retrieval),
		EgressFeeUSD:           round2(egress),
		TotalFeeUSD:            round2(request + retrieval + egress),
		WaitHoursMin:           waitHoursMin,
		WaitHoursMax:           waitHoursMax,
		AlreadyInProgressCount: br.AlreadyInProgressCount,
		AlreadyInProgressBytes: br.AlreadyInProgressBytes,
		AlreadyRestoredCount:   br.AlreadyRestoredCount,
		AlreadyRestoredBytes:   br.AlreadyRestoredBytes,
		UnknownPaths:           unknown,
	})
}

func (s *Server) restoreObjectCount(ctx context.Context, paths []string, allFiles bool) (int64, error) {
	wantAll := allFiles
	wantSet := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if p == "" || p == "/" {
			wantAll = true
			break
		}
		wantSet[p] = struct{}{}
	}

	const pageSize = 1000
	seen := make(map[string]struct{})
	for page := 1; ; page++ {
		rows, _, err := s.deps.DB.ListFiles(ctx, db.FilesFilter{Page: page, Limit: pageSize})
		if err != nil {
			return 0, fmt.Errorf("list files: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, f := range rows {
			if f.Status != db.StatusUploaded && f.Status != db.StatusZipped {
				continue
			}
			if !wantAll {
				hit := false
				for req := range wantSet {
					if pathutil.HasPrefixPath(f.Path, req) {
						hit = true
						break
					}
				}
				if !hit {
					continue
				}
			}
			switch f.RestoreStatus {
			case db.RestoreStatusInProgress, db.RestoreStatusRestored:
				continue
			}
			key := f.S3Key
			if f.ZipName != "" {
				key = f.ZipName
			}
			if key == "" {
				continue
			}
			seen[key] = struct{}{}
		}
		if len(rows) < pageSize {
			break
		}
	}
	return int64(len(seen)), nil
}

type restoreTriggerRequest struct {
	Paths []string `json:"paths"`
	Tier  string   `json:"tier"`
	// Days is how long the restored copy stays in the standard tier
	// before reverting to the archive class. AWS S3 accepts 1..N (N
	// depends on retrieval tier); we clamp to [1, 30] to keep the UI
	// honest and avoid runaway dollar costs from a typo.
	Days int `json:"days"`
}

type restoreTriggerResponse struct {
	KeysRequested         int   `json:"keys_requested"`
	KeysAlreadyInProgress int   `json:"keys_already_in_progress"`
	KeysAlreadyAvailable  int   `json:"keys_already_available"`
	FilesAffected         int64 `json:"files_affected"`
	BytesAffected         int64 `json:"bytes_affected"`
	// Files*Skipped* fields count rows the trigger filtered out because
	// AWS already has (or is producing) a thawed copy. Surfaces in the
	// UI so the operator understands why fewer keys were requested than
	// they may have selected.
	FilesSkippedInProgress int64    `json:"files_skipped_in_progress"`
	BytesSkippedInProgress int64    `json:"bytes_skipped_in_progress"`
	FilesSkippedRestored   int64    `json:"files_skipped_restored"`
	BytesSkippedRestored   int64    `json:"bytes_skipped_restored"`
	UnknownPaths           []string `json:"unknown_paths,omitempty"`
	Errors                 []string `json:"errors,omitempty"`
}

// restoreDaysMin / restoreDaysMax bound the operator-facing days value.
// AWS S3 accepts a wider range, but a 30-day ceiling keeps the dollar
// cost of a typo bounded; the operator can re-issue the restore later
// to extend.
const (
	restoreDaysMin = 1
	restoreDaysMax = 30
)

// handleRestoreTrigger issues a Glacier restore (s3:RestoreObject) for
// every unique S3 key covering the matched DB rows. It does NOT
// download anything — Glacier objects aren't readable until S3 has
// thawed them, which takes hours, so this endpoint just kicks off the
// thaw. Track completion via /api/restore/sync-status (SQS) or a
// /api/restore/scan/* HEAD sweep; the affected DB rows immediately move
// to restore_status='in_progress' so the UI reflects the request.
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
	tier, err := parseRestoreTier(req.Tier)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Days < restoreDaysMin || req.Days > restoreDaysMax {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("days must be in [%d, %d] (got %d)", restoreDaysMin, restoreDaysMax, req.Days))
		return
	}

	var emit engine.EventEmitter
	if s.deps.Bus != nil {
		emit = s.deps.Bus.Publish
	}
	stats, err := engine.RequestRestore(r.Context(), engine.RestoreRequestOptions{
		DB:        s.deps.DB,
		Storage:   st,
		KeyPrefix: s.storagePrefix(),
		Tier:      tier,
		Paths:     req.Paths,
		Days:      req.Days,
		Emit:      emit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, restoreTriggerResponse{
		KeysRequested:          stats.KeysRequested,
		KeysAlreadyInProgress:  stats.KeysAlreadyInProgress,
		KeysAlreadyAvailable:   stats.KeysAlreadyAvailable,
		FilesAffected:          stats.FilesAffected,
		BytesAffected:          stats.BytesAffected,
		FilesSkippedInProgress: stats.FilesSkippedInProgress,
		BytesSkippedInProgress: stats.BytesSkippedInProgress,
		FilesSkippedRestored:   stats.FilesSkippedRestored,
		BytesSkippedRestored:   stats.BytesSkippedRestored,
		UnknownPaths:           stats.UnknownPaths,
		Errors:                 stats.Errors,
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
	// Reject when a backup run is in flight: RunFull HEADs every
	// individually-uploaded key, and the engine may be writing those
	// same s3_key rows (status, uploaded_at). Letting the two writers
	// race opens a corruption window where a stale HEAD response
	// stomps a fresh engine-side state. (#191)
	s.runMu.Lock()
	busy := s.currentRun != 0
	s.runMu.Unlock()
	if busy {
		writeError(w, http.StatusConflict,
			errors.New("a backup run is in progress — full restore-scan would race engine writes; try again when idle"))
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
	// Same engine-idle gate as handleRestoreScanFull — an inventory sync
	// HEADs every key in the manifest, racing engine writes on the same
	// s3_key. (#191)
	s.runMu.Lock()
	busy := s.currentRun != 0
	s.runMu.Unlock()
	if busy {
		writeError(w, http.StatusConflict,
			errors.New("a backup run is in progress — inventory sync would race engine writes; try again when idle"))
		return
	}
	// Serialise concurrent clicks so two callers don't both download a
	// multi-million-key manifest before the scanner's `running` flag
	// rejects the second RunKeys with 409. (#197)
	if !s.inventorySyncBusy.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, errors.New("inventory sync already in progress"))
		return
	}
	defer s.inventorySyncBusy.Store(false)
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
