package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/storage"
)

type downloadSummary struct {
	ID            int64      `json:"id"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Status        string     `json:"status"`
	Phase         string     `json:"phase"`
	DownloadDir   string     `json:"download_dir"`
	Total         int64      `json:"total"`
	TotalBytes    int64      `json:"total_bytes"`
	ObjectCount   int64      `json:"object_count"`
	RequestFeeUSD float64    `json:"request_fee_usd"`
	EgressFeeUSD  float64    `json:"egress_fee_usd"`
	TotalFeeUSD   float64    `json:"total_fee_usd"`
	Scanned       int64      `json:"scanned"`
	Present       int64      `json:"present"`
	Missing       int64      `json:"missing"`
	Processed     int64      `json:"processed"`
	FilesWritten  int64      `json:"files_written"`
	BytesWritten  int64      `json:"bytes_written"`
	Errors        int64      `json:"errors"`
	ErrorMessage  string     `json:"error_message,omitempty"`
}

type downloadTriggerResponse struct {
	DownloadID int64 `json:"download_id"`
}

type downloadMirrorSnapshotSummary struct {
	DownloadDir string    `json:"download_dir"`
	ScannedAt   time.Time `json:"scanned_at"`
	Total       int64     `json:"total"`
	Present     int64     `json:"present"`
	Missing     int64     `json:"missing"`
}

func toDownloadSnapshotSummary(snap db.DownloadMirrorSnapshot) downloadMirrorSnapshotSummary {
	return downloadMirrorSnapshotSummary{
		DownloadDir: snap.DownloadDir,
		ScannedAt:   snap.ScannedAt,
		Total:       snap.TotalCount,
		Present:     snap.PresentCount,
		Missing:     snap.MissingCount,
	}
}

func (s *Server) handleDownloadFull(w http.ResponseWriter, r *http.Request) {
	live, ok := s.snapshotConfig()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	st := s.storage()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("storage not configured"))
		return
	}

	s.runMu.Lock()
	busyRun := s.currentRun != 0
	s.runMu.Unlock()
	if busyRun {
		writeError(w, http.StatusConflict, errors.New("a backup run is in progress — full download would race engine writes; try again when idle"))
		return
	}

	snap, found, err := s.deps.DB.GetDownloadMirrorSnapshot(r.Context(), live.Backup.DownloadDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("load download snapshot: %w", err))
		return
	}

	s.downloadMu.Lock()
	if s.currentDownload != nil {
		cur := *s.currentDownload
		s.downloadMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":          "a download job is already in progress",
			"download_id":    cur.ID,
			"download_phase": cur.Phase,
		})
		return
	}
	job := &downloadSummary{
		ID:          s.downloadSeq.Add(1),
		StartedAt:   time.Now().UTC(),
		Status:      "running",
		Phase:       "download",
		DownloadDir: live.Backup.DownloadDir,
	}
	if !found {
		job.Phase = "scan"
	} else {
		job.Scanned = snap.TotalCount
		job.Present = snap.PresentCount
		job.Missing = snap.MissingCount
	}
	s.currentDownload = job
	s.downloadCancelReq.Store(false)
	s.downloadMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	s.downloadMu.Lock()
	s.currentDownloadCancel = cancel
	s.downloadMu.Unlock()

	emit := func(ev engine.Event) {
		if s.deps.Bus != nil {
			s.deps.Bus.Publish(ev)
		}
		s.applyDownloadEvent(ev)
	}

	var logger *slog.Logger
	if s.deps.Logger != nil {
		logger = s.deps.Logger
	}
	s.downloadWg.Add(1)
	go func(downloadID int64, cfg downloadSummary, storage storage.Storage, tmpDir string) {
		defer s.downloadWg.Done()
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

		stats, err := engine.DownloadMirrorToDir(ctx, engine.DownloadOptions{
			DB:          s.deps.DB,
			Storage:     storage,
			KeyPrefix:   s.storagePrefix(),
			DownloadDir: cfg.DownloadDir,
			TmpDir:      tmpDir,
			Emit:        emit,
		})
		if err != nil {
			cancelled := s.downloadCancelReq.Load()
			if cancelled {
				emit(engine.Event{
					Type: engine.EventDownloadMirrorCancelled,
					At:   time.Now(),
					Data: map[string]any{
						"files_written": stats.FilesWritten,
						"bytes_written": stats.BytesWritten,
						"total_bytes":   stats.TotalBytes,
						"object_count":  stats.ObjectCount,
						"errors":        len(stats.Errors),
						"error":         err.Error(),
					},
				})
			} else {
				emit(engine.Event{
					Type: engine.EventDownloadMirrorFailed,
					At:   time.Now(),
					Data: map[string]any{
						"files_written": stats.FilesWritten,
						"bytes_written": stats.BytesWritten,
						"total_bytes":   stats.TotalBytes,
						"object_count":  stats.ObjectCount,
						"errors":        len(stats.Errors),
						"error":         err.Error(),
					},
				})
			}
		}
		if err != nil && logger != nil {
			logger.Warn("full download failed", "error", err)
		}
		s.downloadMu.Lock()
		if cur := s.currentDownload; cur != nil && cur.ID == downloadID {
			cur.Total = stats.Scanned
			cur.Scanned = stats.Scanned
			cur.Present = stats.Present
			cur.Missing = stats.Missing
			cur.TotalBytes = stats.TotalBytes
			cur.ObjectCount = stats.ObjectCount
			cur.RequestFeeUSD, cur.EgressFeeUSD, cur.TotalFeeUSD = estimateDownloadFees(stats.ObjectCount, stats.TotalBytes)
			cur.FilesWritten = stats.FilesWritten
			cur.BytesWritten = stats.BytesWritten
			cur.Errors = int64(len(stats.Errors))
			if err != nil {
				if s.downloadCancelReq.Load() {
					cur.Status = "cancelled"
					cur.Phase = "cancelled"
					cur.ErrorMessage = ""
				} else {
					cur.Status = "failed"
					cur.Phase = "failed"
					cur.ErrorMessage = err.Error()
				}
			} else {
				cur.Status = "completed"
				cur.Phase = "complete"
				cur.ErrorMessage = ""
			}
			finishedAt := time.Now().UTC()
			final := *cur
			final.FinishedAt = &finishedAt
			s.lastDownload = &final
			s.currentDownload = nil
			s.currentDownloadCancel = nil
			s.downloadCancelReq.Store(false)
		}
		s.downloadMu.Unlock()
	}(job.ID, *job, st, live.Backup.TmpDir)

	writeJSON(w, http.StatusAccepted, downloadTriggerResponse{DownloadID: job.ID})
}

func (s *Server) handleDownloadRescan(w http.ResponseWriter, r *http.Request) {
	live, ok := s.snapshotConfig()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}

	s.runMu.Lock()
	busyRun := s.currentRun != 0
	s.runMu.Unlock()
	if busyRun {
		writeError(w, http.StatusConflict, errors.New("a backup run is in progress — mirror rescan would race engine writes; try again when idle"))
		return
	}

	st := s.storage()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("storage not configured"))
		return
	}

	s.downloadMu.Lock()
	if s.currentDownload != nil {
		cur := *s.currentDownload
		s.downloadMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":          "a download job is already in progress",
			"download_id":    cur.ID,
			"download_phase": cur.Phase,
		})
		return
	}
	job := &downloadSummary{
		ID:          s.downloadSeq.Add(1),
		StartedAt:   time.Now().UTC(),
		Status:      "running",
		Phase:       "scan",
		DownloadDir: live.Backup.DownloadDir,
	}
	s.currentDownload = job
	s.downloadCancelReq.Store(false)
	s.downloadMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	s.downloadMu.Lock()
	s.currentDownloadCancel = cancel
	s.downloadMu.Unlock()

	emit := func(ev engine.Event) {
		if s.deps.Bus != nil {
			s.deps.Bus.Publish(ev)
		}
		s.applyDownloadEvent(ev)
	}

	var logger *slog.Logger
	if s.deps.Logger != nil {
		logger = s.deps.Logger
	}
	s.downloadWg.Add(1)
	go func(downloadID int64, cfg downloadSummary) {
		defer s.downloadWg.Done()
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

		stats, err := engine.ScanDownloadMirror(ctx, engine.DownloadOptions{
			DB:          s.deps.DB,
			Storage:     st,
			KeyPrefix:   s.storagePrefix(),
			DownloadDir: cfg.DownloadDir,
			TmpDir:      live.Backup.TmpDir,
			Emit:        emit,
		})
		if err != nil {
			cancelled := s.downloadCancelReq.Load()
			if cancelled {
				emit(engine.Event{
					Type: engine.EventDownloadMirrorCancelled,
					At:   time.Now(),
					Data: map[string]any{
						"files_written": stats.FilesWritten,
						"bytes_written": stats.BytesWritten,
						"total_bytes":   stats.TotalBytes,
						"object_count":  stats.ObjectCount,
						"errors":        len(stats.Errors),
						"error":         err.Error(),
					},
				})
			} else {
				emit(engine.Event{
					Type: engine.EventDownloadMirrorFailed,
					At:   time.Now(),
					Data: map[string]any{
						"files_written": stats.FilesWritten,
						"bytes_written": stats.BytesWritten,
						"total_bytes":   stats.TotalBytes,
						"object_count":  stats.ObjectCount,
						"errors":        len(stats.Errors),
						"error":         err.Error(),
					},
				})
			}
		}
		if err != nil && logger != nil {
			logger.Warn("mirror rescan failed", "error", err)
		}
		s.downloadMu.Lock()
		if cur := s.currentDownload; cur != nil && cur.ID == downloadID {
			cur.Total = stats.Scanned
			cur.Scanned = stats.Scanned
			cur.Present = stats.Present
			cur.Missing = stats.Missing
			cur.TotalBytes = 0
			cur.ObjectCount = 0
			cur.RequestFeeUSD, cur.EgressFeeUSD, cur.TotalFeeUSD = 0, 0, 0
			cur.FilesWritten = 0
			cur.BytesWritten = 0
			cur.Errors = int64(len(stats.Errors))
			if err != nil {
				if s.downloadCancelReq.Load() {
					cur.Status = "cancelled"
					cur.Phase = "cancelled"
					cur.ErrorMessage = ""
				} else {
					cur.Status = "failed"
					cur.Phase = "failed"
					cur.ErrorMessage = err.Error()
				}
			} else {
				cur.Status = "completed"
				cur.Phase = "complete"
				cur.ErrorMessage = ""
			}
			finishedAt := time.Now().UTC()
			final := *cur
			final.FinishedAt = &finishedAt
			s.lastDownload = &final
			s.currentDownload = nil
			s.currentDownloadCancel = nil
			s.downloadCancelReq.Store(false)
		}
		s.downloadMu.Unlock()
	}(job.ID, *job)

	writeJSON(w, http.StatusAccepted, downloadTriggerResponse{DownloadID: job.ID})
}

func (s *Server) applyDownloadEvent(ev engine.Event) {
	s.downloadMu.Lock()
	defer s.downloadMu.Unlock()
	cur := s.currentDownload
	if cur == nil {
		return
	}
	switch ev.Type {
	case engine.EventDownloadMirrorScanStart:
		cur.Status = "running"
		cur.Phase = "scan"
		cur.Total = intFromAny(ev.Data["total"])
		cur.Scanned = 0
		cur.Present = 0
		cur.Missing = 0
	case engine.EventDownloadMirrorScanProgress:
		cur.Status = "running"
		cur.Phase = "scan"
		cur.Scanned = intFromAny(ev.Data["scanned"])
		cur.Present = intFromAny(ev.Data["present"])
		cur.Missing = intFromAny(ev.Data["missing"])
		cur.Total = intFromAny(ev.Data["total"])
	case engine.EventDownloadMirrorScanComplete:
		downloadAfter := boolFromAny(ev.Data["download_after"])
		cur.Scanned = intFromAny(ev.Data["scanned"])
		cur.Present = intFromAny(ev.Data["present"])
		cur.Missing = intFromAny(ev.Data["missing"])
		cur.Total = intFromAny(ev.Data["total"])
		if totalBytes := intFromAny(ev.Data["total_bytes"]); totalBytes > 0 {
			cur.TotalBytes = totalBytes
		}
		if objectCount := intFromAny(ev.Data["object_count"]); objectCount > 0 {
			cur.ObjectCount = objectCount
			cur.RequestFeeUSD, cur.EgressFeeUSD, cur.TotalFeeUSD = estimateDownloadFees(cur.ObjectCount, cur.TotalBytes)
		}
		if downloadAfter {
			cur.Status = "running"
			cur.Phase = "download"
		} else {
			cur.Status = "completed"
			cur.Phase = "complete"
			finishedAt := time.Now().UTC()
			cur.FinishedAt = &finishedAt
		}
	case engine.EventDownloadMirrorScanFailed:
		cur.Status = "failed"
		cur.Phase = "failed"
		cur.ErrorMessage = stringFromAny(ev.Data["error"])
		finishedAt := time.Now().UTC()
		cur.FinishedAt = &finishedAt
	case engine.EventDownloadMirrorStart:
		cur.Status = "running"
		cur.Phase = "download"
		cur.Total = intFromAny(ev.Data["total"])
		cur.TotalBytes = intFromAny(ev.Data["total_bytes"])
		cur.ObjectCount = intFromAny(ev.Data["object_count"])
		cur.RequestFeeUSD, cur.EgressFeeUSD, cur.TotalFeeUSD = estimateDownloadFees(cur.ObjectCount, cur.TotalBytes)
		cur.Processed = 0
		cur.FilesWritten = 0
		cur.BytesWritten = 0
		cur.Errors = 0
	case engine.EventDownloadMirrorProgress:
		cur.Status = "running"
		cur.Phase = "download"
		cur.Processed = intFromAny(ev.Data["processed"])
		cur.Total = intFromAny(ev.Data["total"])
		cur.TotalBytes = intFromAny(ev.Data["total_bytes"])
		cur.ObjectCount = intFromAny(ev.Data["object_count"])
		cur.RequestFeeUSD, cur.EgressFeeUSD, cur.TotalFeeUSD = estimateDownloadFees(cur.ObjectCount, cur.TotalBytes)
		cur.FilesWritten = intFromAny(ev.Data["files_written"])
		cur.BytesWritten = intFromAny(ev.Data["bytes_written"])
		cur.Errors = intFromAny(ev.Data["errors"])
	case engine.EventDownloadMirrorComplete:
		cur.Status = "completed"
		cur.Phase = "complete"
		cur.TotalBytes = intFromAny(ev.Data["total_bytes"])
		cur.ObjectCount = intFromAny(ev.Data["object_count"])
		cur.RequestFeeUSD, cur.EgressFeeUSD, cur.TotalFeeUSD = estimateDownloadFees(cur.ObjectCount, cur.TotalBytes)
		cur.FilesWritten = intFromAny(ev.Data["files_written"])
		cur.BytesWritten = intFromAny(ev.Data["bytes_written"])
		cur.Errors = intFromAny(ev.Data["errors"])
		finishedAt := time.Now().UTC()
		cur.FinishedAt = &finishedAt
	case engine.EventDownloadMirrorFailed:
		cur.Status = "failed"
		cur.Phase = "failed"
		cur.TotalBytes = intFromAny(ev.Data["total_bytes"])
		cur.ObjectCount = intFromAny(ev.Data["object_count"])
		cur.RequestFeeUSD, cur.EgressFeeUSD, cur.TotalFeeUSD = estimateDownloadFees(cur.ObjectCount, cur.TotalBytes)
		cur.FilesWritten = intFromAny(ev.Data["files_written"])
		cur.BytesWritten = intFromAny(ev.Data["bytes_written"])
		cur.Errors = intFromAny(ev.Data["errors"])
		cur.ErrorMessage = stringFromAny(ev.Data["error"])
		finishedAt := time.Now().UTC()
		cur.FinishedAt = &finishedAt
	case engine.EventDownloadMirrorCancelled:
		cur.Status = "cancelled"
		cur.Phase = "cancelled"
		cur.TotalBytes = intFromAny(ev.Data["total_bytes"])
		cur.ObjectCount = intFromAny(ev.Data["object_count"])
		cur.RequestFeeUSD, cur.EgressFeeUSD, cur.TotalFeeUSD = estimateDownloadFees(cur.ObjectCount, cur.TotalBytes)
		cur.FilesWritten = intFromAny(ev.Data["files_written"])
		cur.BytesWritten = intFromAny(ev.Data["bytes_written"])
		cur.Errors = intFromAny(ev.Data["errors"])
		cur.ErrorMessage = ""
		finishedAt := time.Now().UTC()
		cur.FinishedAt = &finishedAt
	}
}

func (s *Server) handleDownloadCancel(w http.ResponseWriter, r *http.Request) {
	s.downloadMu.Lock()
	cur := s.currentDownload
	cancel := s.currentDownloadCancel
	s.downloadMu.Unlock()
	if cur == nil || cancel == nil {
		writeError(w, http.StatusNotFound, errors.New("no download job is in progress"))
		return
	}
	s.downloadCancelReq.Store(true)
	cancel()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

func boolFromAny(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true" || b == "1"
	case int:
		return b != 0
	case int8:
		return b != 0
	case int16:
		return b != 0
	case int32:
		return b != 0
	case int64:
		return b != 0
	case uint:
		return b != 0
	case uint8:
		return b != 0
	case uint16:
		return b != 0
	case uint32:
		return b != 0
	case uint64:
		return b != 0
	default:
		return false
	}
}

func (s *Server) loadDownloadMirrorSnapshotSummary(ctx context.Context) (*downloadMirrorSnapshotSummary, error) {
	live, ok := s.snapshotConfig()
	if !ok {
		return nil, nil
	}
	snap, found, err := s.deps.DB.GetDownloadMirrorSnapshot(ctx, live.Backup.DownloadDir)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	sum := toDownloadSnapshotSummary(snap)
	return &sum, nil
}

func intFromAny(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int8:
		return int64(n)
	case int16:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case uint:
		return int64(n)
	case uint8:
		return int64(n)
	case uint16:
		return int64(n)
	case uint32:
		return int64(n)
	case uint64:
		return int64(n)
	case float64:
		return int64(n)
	case float32:
		return int64(n)
	default:
		return 0
	}
}

func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
