package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

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
	pricePerThousandRequestsStandard = 0.11   // USD; Glacier standard retrieval requests
	pricePerGBRetrievalStandard      = 0.02   // USD/GB; Glacier standard retrieval
	pricePerThousandRequestsBulk     = 0.025  // USD; Glacier bulk retrieval requests
	pricePerGBRetrievalBulk          = 0.003  // USD/GB; Glacier bulk retrieval
	pricePerGBStorageStandard        = 0.023  // USD/GB-month; restored copy billed at S3 Standard rates
	pricePerThousandRequestsGet      = 0.0004 // USD; S3 Standard GET requests
	pricePerGBEgress                 = 0.09   // USD/GB; internet egress, pessimistic estimate uses the full byte total
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

func estimateDownloadFees(objectCount, totalBytes int64) (requestFeeUSD, egressFeeUSD, totalFeeUSD float64) {
	requestFeeUSD = float64(objectCount) * pricePerThousandRequestsGet / 1000
	gb := float64(totalBytes) / (1024 * 1024 * 1024)
	// Use the full byte total here so the dashboard shows the maximal
	// outbound cost the operator may be on the hook for.
	egressFeeUSD = gb * pricePerGBEgress
	totalFeeUSD = requestFeeUSD + egressFeeUSD
	return requestFeeUSD, egressFeeUSD, totalFeeUSD
}

func estimateRestoreStorageFee(totalBytes int64, days int) float64 {
	if totalBytes <= 0 || days <= 0 {
		return 0
	}
	gb := float64(totalBytes) / (1024 * 1024 * 1024)
	months := float64(days) / 30.0
	return gb * months * pricePerGBStorageStandard
}

type restoreEstimateRequest struct {
	Paths []string `json:"paths"`
	Tier  string   `json:"tier"`
	Days  int      `json:"days"`
}

type restoreEstimateResponse struct {
	// FileCount / TotalBytes count only the actual S3 objects that will
	// be retrieved — i.e. one zip archive per zip group, plus standalone
	// objects. Rows already in_progress or restored are excluded. The
	// estimate prices request, retrieval, temporary S3 Standard storage,
	// and egress separately; TotalBytes remains the current DB-byte
	// estimate for the matching rows.
	FileCount       int64   `json:"file_count"`
	TotalBytes      int64   `json:"total_bytes"`
	RequestFeeUSD   float64 `json:"request_fee_usd"`
	RetrievalFeeUSD float64 `json:"retrieval_fee_usd"`
	StorageFeeUSD   float64 `json:"storage_fee_usd"`
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
	if req.Days < restoreDaysMin || req.Days > restoreDaysMax {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("days must be in [%d, %d] (got %d)", restoreDaysMin, restoreDaysMax, req.Days))
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
	storage := estimateRestoreStorageFee(br.RetrievableBytes, req.Days)
	// Use the full byte total here so the estimate stays pessimistic
	// and leaves any AWS free-tier savings for the operator to verify.
	egress := gb * pricePerGBEgress

	writeJSON(w, http.StatusOK, restoreEstimateResponse{
		FileCount:              objectCount,
		TotalBytes:             br.RetrievableBytes,
		RequestFeeUSD:          round2(request),
		RetrievalFeeUSD:        round2(retrieval),
		StorageFeeUSD:          round2(storage),
		EgressFeeUSD:           round2(egress),
		TotalFeeUSD:            round2(request + retrieval + storage + egress),
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
			if f.Status != db.StatusUploaded && f.Status != db.StatusZipped && f.Status != db.StatusCloudOnly {
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
	// before reverting to the archive class. AWS S3 accepts a wider
	// range; we clamp to [1, 180] to keep the UI honest and avoid
	// runaway dollar costs from a typo.
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

type restoreDownloadRequest struct {
	Paths          []string `json:"paths"`
	TargetDir      string   `json:"target_dir"`
	VerifyChecksum *bool    `json:"verify_checksum,omitempty"`
}

type restoreDownloadSummary struct {
	ID           int64      `json:"id"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Status       string     `json:"status"`
	Phase        string     `json:"phase"`
	TargetDir    string     `json:"target_dir"`
	Total        int64      `json:"total"`
	TotalBytes   int64      `json:"total_bytes"`
	Processed    int64      `json:"processed"`
	FilesWritten int64      `json:"files_written"`
	BytesWritten int64      `json:"bytes_written"`
	Errors       int64      `json:"errors"`
	ErrorMessage string     `json:"error_message,omitempty"`
}

type restoreDownloadResponse struct {
	FilesWritten int64    `json:"files_written"`
	BytesWritten int64    `json:"bytes_written"`
	TotalBytes   int64    `json:"total_bytes"`
	Skipped      []string `json:"skipped,omitempty"`
	Errors       []string `json:"errors,omitempty"`
}

// restoreToDirResponse is the legacy test name for restoreDownloadResponse.
// Keep it as an alias so older callers keep compiling while the API surface
// uses the /restore/download naming everywhere else.
type restoreToDirResponse = restoreDownloadResponse

type restoreDownloadEstimateRequest struct {
	Paths []string `json:"paths"`
}

type restoreDownloadEstimateResponse struct {
	ObjectCount       int64    `json:"object_count"`
	TotalBytes        int64    `json:"total_bytes"`
	RestoredCount     int64    `json:"restored_count"`
	InProgressCount   int64    `json:"in_progress_count"`
	NotRestoringCount int64    `json:"not_restoring_count"`
	RequestFeeUSD     float64  `json:"request_fee_usd"`
	EgressFeeUSD      float64  `json:"egress_fee_usd"`
	TotalFeeUSD       float64  `json:"total_fee_usd"`
	UnknownPaths      []string `json:"unknown_paths,omitempty"`
}

// restoreDaysMin / restoreDaysMax bound the operator-facing days value.
// The restored copy is billed at S3 Standard rates for the full period,
// so the 180-day ceiling keeps typo-driven costs bounded while still
// covering the longest Deep Archive retention window.
const (
	restoreDaysMin = 1
	restoreDaysMax = 180
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

// handleRestoreDownload starts a background job that downloads matching
// files from S3 into a local directory and verifies each written file
// against the MD5 stored in the DB. Only rows currently marked
// restore_status='restored' are treated as downloadable; thaw-pending
// and never-restored rows are reported as skipped. Progress is exposed
// through SSE plus /api/status so the Download tab can survive reloads.
func (s *Server) handleRestoreDownload(w http.ResponseWriter, r *http.Request) {
	st := s.storage()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("storage not configured"))
		return
	}

	var req restoreDownloadRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("paths must be non-empty"))
		return
	}
	if req.TargetDir == "" || !filepath.IsAbs(req.TargetDir) {
		writeError(w, http.StatusBadRequest, errors.New("target_dir must be an absolute path"))
		return
	}

	s.runMu.Lock()
	busy := s.currentRun != 0
	s.runMu.Unlock()
	if busy {
		writeError(w, http.StatusConflict,
			errors.New("a backup run is in progress — download would race engine writes; try again when idle"))
		return
	}

	cfg, ok := s.snapshotConfig()
	tmpDir := ""
	if ok {
		tmpDir = cfg.Backup.TmpDir
	}

	s.restoreDownloadMu.Lock()
	if s.currentRestoreDownload != nil {
		cur := *s.currentRestoreDownload
		s.restoreDownloadMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":                  "a restore download is already in progress",
			"restore_download_id":    cur.ID,
			"restore_download_phase": cur.Phase,
		})
		return
	}
	job := &restoreDownloadSummary{
		ID:        s.restoreDownloadSeq.Add(1),
		StartedAt: time.Now().UTC(),
		Status:    "running",
		Phase:     "download",
		TargetDir: req.TargetDir,
	}
	s.currentRestoreDownload = job
	s.restoreDownloadMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	s.restoreDownloadMu.Lock()
	s.currentRestoreDownloadCancel = cancel
	s.restoreDownloadMu.Unlock()

	apply := func(ev engine.Event) {
		if s.deps.Bus != nil {
			s.deps.Bus.Publish(ev)
		}
		s.applyRestoreDownloadEvent(ev)
	}

	var logger *slog.Logger
	if s.deps.Logger != nil {
		logger = s.deps.Logger
	}

	s.restoreDownloadWg.Add(1)
	go func(downloadID int64, cfg restoreDownloadSummary, storage storage.Storage, tmpDir string) {
		defer s.restoreDownloadWg.Done()
		defer cancel()
		done := make(chan struct{})
		go func() {
			select {
			case <-s.shutdownCh:
				cancel()
			case <-done:
			}
		}()
		defer close(done)

		stats, err := engine.RestoreToDir(ctx, engine.RestoreOptions{
			DB:           s.deps.DB,
			Storage:      storage,
			KeyPrefix:    s.storagePrefix(),
			TargetDir:    cfg.TargetDir,
			Paths:        req.Paths,
			TmpDir:       tmpDir,
			SkipChecksum: req.VerifyChecksum != nil && !*req.VerifyChecksum,
			Emit:         apply,
		})
		if err != nil {
			if logger != nil {
				logger.Warn("restore download failed", "error", err)
			}
		}
		s.restoreDownloadMu.Lock()
		if cur := s.currentRestoreDownload; cur != nil && cur.ID == downloadID {
			cur.TotalBytes = stats.TotalBytes
			cur.Processed = stats.FilesWritten
			cur.FilesWritten = stats.FilesWritten
			cur.BytesWritten = stats.BytesWritten
			cur.Errors = int64(len(stats.Errors))
			if err != nil {
				cur.Status = "failed"
				cur.Phase = "failed"
				cur.ErrorMessage = err.Error()
			} else {
				cur.Status = "completed"
				cur.Phase = "complete"
				cur.ErrorMessage = ""
			}
			finishedAt := time.Now().UTC()
			final := *cur
			final.FinishedAt = &finishedAt
			s.lastRestoreDownload = &final
			s.currentRestoreDownload = nil
			s.currentRestoreDownloadCancel = nil
		}
		s.restoreDownloadMu.Unlock()
	}(job.ID, *job, st, tmpDir)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"restore_download_id": job.ID,
	})
}

func (s *Server) applyRestoreDownloadEvent(ev engine.Event) {
	s.restoreDownloadMu.Lock()
	defer s.restoreDownloadMu.Unlock()
	cur := s.currentRestoreDownload
	if cur == nil {
		return
	}
	switch ev.Type {
	case engine.EventRestoreDownloadStart:
		cur.Status = "running"
		cur.Phase = "download"
		cur.Total = intFromAny(ev.Data["total"])
		cur.TotalBytes = intFromAny(ev.Data["total_bytes"])
		cur.Processed = 0
		cur.FilesWritten = 0
		cur.BytesWritten = 0
		cur.Errors = 0
	case engine.EventRestoreDownloadProgress:
		cur.Status = "running"
		cur.Phase = "download"
		cur.Processed = intFromAny(ev.Data["processed"])
		cur.Total = intFromAny(ev.Data["total"])
		cur.TotalBytes = intFromAny(ev.Data["total_bytes"])
		cur.FilesWritten = intFromAny(ev.Data["files_written"])
		cur.BytesWritten = intFromAny(ev.Data["bytes_written"])
		cur.Errors = intFromAny(ev.Data["errors"])
	case engine.EventRestoreDownloadComplete:
		cur.Status = "completed"
		cur.Phase = "complete"
		cur.TotalBytes = intFromAny(ev.Data["total_bytes"])
		cur.FilesWritten = intFromAny(ev.Data["files_written"])
		cur.Processed = cur.FilesWritten
		cur.BytesWritten = intFromAny(ev.Data["bytes_written"])
		cur.Errors = intFromAny(ev.Data["errors"])
		finishedAt := time.Now().UTC()
		cur.FinishedAt = &finishedAt
	case engine.EventRestoreDownloadFailed:
		cur.Status = "failed"
		cur.Phase = "failed"
		cur.TotalBytes = intFromAny(ev.Data["total_bytes"])
		cur.FilesWritten = intFromAny(ev.Data["files_written"])
		cur.Processed = cur.FilesWritten
		cur.BytesWritten = intFromAny(ev.Data["bytes_written"])
		cur.Errors = intFromAny(ev.Data["errors"])
		cur.ErrorMessage = stringFromAny(ev.Data["error"])
		finishedAt := time.Now().UTC()
		cur.FinishedAt = &finishedAt
	}
}

// handleRestoreToDir is the legacy handler name for the restore download
// endpoint. Keep it as a shim so older tests and callers continue to work
// while the route stays on /restore/download.
func (s *Server) handleRestoreToDir(w http.ResponseWriter, r *http.Request) {
	s.handleRestoreDownload(w, r)
}

// handleRestoreDownloadEstimate computes a download cost estimate from DB
// metadata only. It counts only rows currently marked restored as
// downloadable, splits the rest into in-progress vs not-restoring, and
// prices the actual S3 objects that will be fetched (one zip archive per
// restored zip group, plus standalone objects) from the indexed file sizes.
func (s *Server) handleRestoreDownloadEstimate(w http.ResponseWriter, r *http.Request) {
	var req restoreDownloadEstimateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("paths must be non-empty"))
		return
	}

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

	br, err := s.downloadEstimateStats(r.Context(), pathsForFilter, allFiles)
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

	request, egress, total := estimateDownloadFees(br.ObjectCount, br.TotalBytes)

	writeJSON(w, http.StatusOK, restoreDownloadEstimateResponse{
		ObjectCount:       br.ObjectCount,
		TotalBytes:        br.TotalBytes,
		RestoredCount:     br.RestoredCount,
		InProgressCount:   br.InProgressCount,
		NotRestoringCount: br.NotRestoringCount,
		RequestFeeUSD:     round2(request),
		EgressFeeUSD:      round2(egress),
		TotalFeeUSD:       round2(total),
		UnknownPaths:      unknown,
	})
}

type downloadEstimateBreakdown struct {
	ObjectCount       int64
	TotalBytes        int64
	RestoredCount     int64
	InProgressCount   int64
	NotRestoringCount int64
	MatchedPaths      []string
}

func (s *Server) downloadEstimateStats(ctx context.Context, paths []string, allFiles bool) (downloadEstimateBreakdown, error) {
	var br downloadEstimateBreakdown
	statuses := []string{db.StatusUploaded, db.StatusZipped, db.StatusCloudOnly}
	wantAll := allFiles
	wantSet := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if p == "" || p == "/" {
			wantAll = true
			break
		}
		wantSet[p] = struct{}{}
	}

	seenKeys := make(map[string]struct{})
	matched := make(map[string]bool)
	const pageSize = 1000
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return downloadEstimateBreakdown{}, err
		}
		rows, _, err := s.deps.DB.ListFiles(ctx, db.FilesFilter{Page: page, Limit: pageSize})
		if err != nil {
			return downloadEstimateBreakdown{}, fmt.Errorf("list files: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, f := range rows {
			if f.Status != statuses[0] && f.Status != statuses[1] {
				continue
			}
			if !wantAll {
				hit := false
				for req := range wantSet {
					if pathutil.HasPrefixPath(f.Path, req) {
						hit = true
						matched[req] = true
						break
					}
				}
				if !hit {
					continue
				}
			}
			switch f.RestoreStatus {
			case db.RestoreStatusRestored:
				br.RestoredCount++
				br.TotalBytes += f.Size
			case db.RestoreStatusInProgress:
				br.InProgressCount++
			default:
				br.NotRestoringCount++
			}
			if f.RestoreStatus != db.RestoreStatusRestored {
				continue
			}
			key := f.S3Key
			if f.ZipName != "" {
				key = pathutil.JoinKey(s.storagePrefix(), f.ZipName)
			}
			if key == "" {
				continue
			}
			if _, ok := seenKeys[key]; !ok {
				seenKeys[key] = struct{}{}
				br.ObjectCount++
			}
		}
		if len(rows) < pageSize {
			break
		}
	}

	br.MatchedPaths = make([]string, 0, len(paths))
	for _, p := range paths {
		if matched[p] {
			br.MatchedPaths = append(br.MatchedPaths, p)
		}
	}
	return br, nil
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
