package api

import (
	"context"
	"time"

	"github.com/Wlczak/aws-backup/internal/engine"
)

const (
	restoreJobKindTrigger   = "trigger"
	restoreJobKindInventory = "inventory"
)

// restoreJobSummary captures the live/terminal state for restore-trigger
// and inventory-sync jobs so /api/status and /api/restore/jobs/{id} can
// recover progress after a page refresh.
type restoreJobSummary struct {
	ID         int64      `json:"id"`
	Kind       string     `json:"kind"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Status     string     `json:"status"`
	Phase      string     `json:"phase"`
	Total      int64      `json:"total"`
	Processed  int64      `json:"processed"`
	Scanned    int64      `json:"scanned"`
	Updated    int64      `json:"updated"`
	Errors     int64      `json:"errors"`

	KeysRequested          int      `json:"keys_requested,omitempty"`
	KeysAlreadyInProgress  int      `json:"keys_already_in_progress,omitempty"`
	KeysAlreadyAvailable   int      `json:"keys_already_available,omitempty"`
	FilesAffected          int64    `json:"files_affected,omitempty"`
	BytesAffected          int64    `json:"bytes_affected,omitempty"`
	FilesSkippedInProgress int64    `json:"files_skipped_in_progress,omitempty"`
	BytesSkippedInProgress int64    `json:"bytes_skipped_in_progress,omitempty"`
	FilesSkippedRestored   int64    `json:"files_skipped_restored,omitempty"`
	BytesSkippedRestored   int64    `json:"bytes_skipped_restored,omitempty"`
	UnknownPaths           []string `json:"unknown_paths,omitempty"`
	ManifestKey            string   `json:"manifest_key,omitempty"`
	ErrorMessage           string   `json:"error_message,omitempty"`
}

func (s *Server) snapshotRestoreJob() (current *restoreJobSummary, last *restoreJobSummary) {
	s.restoreJobMu.Lock()
	defer s.restoreJobMu.Unlock()
	if s.currentRestoreJob != nil {
		cur := *s.currentRestoreJob
		current = &cur
	}
	if s.lastRestoreJob != nil {
		prev := *s.lastRestoreJob
		last = &prev
	}
	return current, last
}

func cloneRestoreJob(job *restoreJobSummary) *restoreJobSummary {
	if job == nil {
		return nil
	}
	copy := *job
	return &copy
}

func (s *Server) lookupRestoreJob(id int64) *restoreJobSummary {
	s.restoreJobMu.Lock()
	defer s.restoreJobMu.Unlock()
	if s.currentRestoreJob != nil && s.currentRestoreJob.ID == id {
		return cloneRestoreJob(s.currentRestoreJob)
	}
	if s.lastRestoreJob != nil && s.lastRestoreJob.ID == id {
		return cloneRestoreJob(s.lastRestoreJob)
	}
	return nil
}

func (s *Server) startRestoreJob(kind string) (*restoreJobSummary, *restoreJobSummary) {
	s.restoreJobMu.Lock()
	defer s.restoreJobMu.Unlock()
	if s.currentRestoreJob != nil {
		return nil, cloneRestoreJob(s.currentRestoreJob)
	}
	job := &restoreJobSummary{
		ID:        s.restoreJobSeq.Add(1),
		Kind:      kind,
		StartedAt: time.Now().UTC(),
		Status:    "running",
		Phase:     "starting",
	}
	s.currentRestoreJob = job
	return cloneRestoreJob(job), nil
}

func (s *Server) updateRestoreJob(jobID int64, fn func(*restoreJobSummary)) {
	s.restoreJobMu.Lock()
	defer s.restoreJobMu.Unlock()
	if s.currentRestoreJob == nil || s.currentRestoreJob.ID != jobID {
		return
	}
	fn(s.currentRestoreJob)
}

func (s *Server) finishRestoreJob(jobID int64, status string, err error) {
	s.restoreJobMu.Lock()
	defer s.restoreJobMu.Unlock()
	if s.currentRestoreJob == nil || s.currentRestoreJob.ID != jobID {
		return
	}
	job := s.currentRestoreJob
	job.Status = status
	if status == "completed" {
		job.Phase = "complete"
	} else if status == "cancelled" {
		job.Phase = "cancelled"
	} else {
		job.Phase = "failed"
	}
	if err != nil {
		job.ErrorMessage = err.Error()
	} else {
		job.ErrorMessage = ""
	}
	finishedAt := time.Now().UTC()
	job.FinishedAt = &finishedAt
	final := *job
	s.lastRestoreJob = &final
	s.currentRestoreJob = nil
	s.currentRestoreJobCancel = nil
}

func (s *Server) emitRestoreJobEvent(jobID int64, ev engine.Event) {
	if s.deps.Bus != nil {
		s.deps.Bus.Publish(ev)
	}
	s.updateRestoreJob(jobID, func(job *restoreJobSummary) {
		switch ev.Type {
		case engine.EventRestoreRequestStart:
			job.Kind = restoreJobKindTrigger
			job.Status = "running"
			job.Phase = "request"
			job.Total = intFromAny(ev.Data["total"])
			job.Processed = 0
			job.KeysRequested = 0
			job.KeysAlreadyInProgress = 0
			job.KeysAlreadyAvailable = 0
			job.FilesAffected = 0
			job.BytesAffected = 0
			job.Errors = 0
		case engine.EventRestoreRequestProgress:
			job.Status = "running"
			job.Phase = "request"
			job.Processed = intFromAny(ev.Data["processed"])
			job.Total = intFromAny(ev.Data["total"])
			job.KeysRequested = int(intFromAny(ev.Data["keys_requested"]))
			job.Errors = intFromAny(ev.Data["errors"])
		case engine.EventRestoreRequestComplete:
			job.Status = "completed"
			job.Phase = "complete"
			job.Total = intFromAny(ev.Data["total"])
			job.Processed = job.Total
			job.KeysRequested = int(intFromAny(ev.Data["keys_requested"]))
			job.KeysAlreadyInProgress = int(intFromAny(ev.Data["keys_already_in_progress"]))
			job.KeysAlreadyAvailable = int(intFromAny(ev.Data["keys_already_available"]))
			job.FilesAffected = intFromAny(ev.Data["files_affected"])
			job.BytesAffected = intFromAny(ev.Data["bytes_affected"])
			job.Errors = intFromAny(ev.Data["errors"])
			finishedAt := time.Now().UTC()
			job.FinishedAt = &finishedAt
		case engine.EventRestoreRequestFailed:
			job.Status = restoreJobStatusFromError(ev.Data["error"])
			job.Phase = "failed"
			job.Processed = intFromAny(ev.Data["processed"])
			job.Total = intFromAny(ev.Data["total"])
			job.ErrorMessage = stringFromAny(ev.Data["error"])
			job.Errors = 1
			finishedAt := time.Now().UTC()
			job.FinishedAt = &finishedAt
		case engine.EventRestoreScanStart:
			job.Status = "running"
			job.Phase = "scan"
			job.Total = intFromAny(ev.Data["total"])
			job.Processed = 0
			job.Scanned = 0
			job.Updated = 0
			job.Errors = 0
		case engine.EventRestoreScanProgress:
			job.Status = "running"
			job.Phase = "scan"
			job.Processed = intFromAny(ev.Data["scanned"])
			job.Scanned = intFromAny(ev.Data["scanned"])
			job.Updated = intFromAny(ev.Data["updated"])
			job.Errors = intFromAny(ev.Data["errors"])
			if total := intFromAny(ev.Data["total"]); total > 0 {
				job.Total = total
			}
		case engine.EventRestoreScanComplete:
			job.Status = "completed"
			job.Phase = "complete"
			job.Scanned = intFromAny(ev.Data["scanned"])
			job.Updated = intFromAny(ev.Data["updated"])
			job.Errors = intFromAny(ev.Data["errors"])
			job.Processed = job.Scanned
			finishedAt := time.Now().UTC()
			job.FinishedAt = &finishedAt
		case engine.EventRestoreScanFailed:
			job.Status = restoreJobStatusFromError(ev.Data["error"])
			job.Phase = "failed"
			job.ErrorMessage = stringFromAny(ev.Data["error"])
			job.Errors = 1
			finishedAt := time.Now().UTC()
			job.FinishedAt = &finishedAt
		case engine.EventRestoreManifestStart:
			job.Status = "running"
			job.Phase = "manifest"
			job.Total = intFromAny(ev.Data["total"])
			job.Processed = intFromAny(ev.Data["processed"])
			job.ManifestKey = stringFromAny(ev.Data["manifest_key"])
			job.Errors = 0
		case engine.EventRestoreManifestProgress:
			job.Status = "running"
			job.Phase = "manifest"
			job.Total = intFromAny(ev.Data["total"])
			job.Processed = intFromAny(ev.Data["processed"])
			job.ManifestKey = stringFromAny(ev.Data["manifest_key"])
			job.Errors = intFromAny(ev.Data["errors"])
		case engine.EventRestoreManifestComplete:
			job.Status = "running"
			job.Phase = "scan"
			job.Total = intFromAny(ev.Data["total"])
			job.Processed = intFromAny(ev.Data["processed"])
			job.ManifestKey = stringFromAny(ev.Data["manifest_key"])
			job.Errors = intFromAny(ev.Data["errors"])
		case engine.EventRestoreManifestFailed:
			job.Status = restoreJobStatusFromError(ev.Data["error"])
			job.Phase = "failed"
			job.ErrorMessage = stringFromAny(ev.Data["error"])
			job.Errors = 1
			finishedAt := time.Now().UTC()
			job.FinishedAt = &finishedAt
		}
	})
}

func restoreJobStatusFromError(v any) string {
	msg := stringFromAny(v)
	if msg == context.Canceled.Error() {
		return "cancelled"
	}
	return "failed"
}
