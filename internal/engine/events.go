package engine

import "time"

// Event is a typed progress event emitted during a backup run. The Data
// field is a map so it serialises cleanly to JSON for SSE without
// coupling this package to the API layer.
type Event struct {
	Type      string         `json:"type"`
	RunID     int64          `json:"run_id,omitempty"`
	At        time.Time      `json:"at"`
	Data      map[string]any `json:"data,omitempty"`
}

// Event type constants — keep in sync with plan.md and the SSE layer.
const (
	EventScanProgress    = "scan_progress"
	EventScanComplete    = "scan_complete"
	EventUploadPlan      = "upload_plan"
	EventCopyProgress    = "copy_progress"
	// EventRunLog is emitted as a burst on SSE connect to replay the
	// in-flight run's persisted log lines, so clients that reconnect
	// mid-run see the full history rather than just events from the
	// moment of reconnection. (#130)
	EventRunLog          = "run_log"
	EventUploadStart     = "upload_start"
	EventUploadProgress  = "upload_progress"
	EventUploadComplete  = "upload_complete"
	EventUploadFailed    = "upload_failed"
	EventRunStart        = "run_start"
	EventRunComplete     = "run_complete"
)

// EventEmitter is the minimal sink the engine uses to announce progress.
// Feature 8 replaces the default emitter with a real pub/sub bus; for
// now tests wire a channel-based sink directly.
type EventEmitter func(Event)

// DiscardEvents is a no-op emitter for tests that don't care.
func DiscardEvents(_ Event) {}
