package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/Wlczak/aws-backup/internal/config"
)

// settingsResponse embeds config.Config so its fields are promoted to the
// top of the JSON object; PendingApply is added alongside. Callers that
// decode into a plain config.Config still work because the JSON decoder
// ignores the extra field.
type settingsResponse struct {
	config.Config
	PendingApply bool `json:"pending_apply"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	live, ok := s.snapshotConfig()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}

	s.runMu.Lock()
	pending := s.pendingConfig
	s.runMu.Unlock()

	cfg := live
	if pending != nil {
		cfg = *pending
	}
	writeJSON(w, http.StatusOK, settingsResponse{
		Config:       cfg.Redacted(),
		PendingApply: pending != nil,
	})
}

// handlePutSettings accepts a full Config in the body. Fields equal to
// RedactedMarker ("***") preserve the existing value so clients that GET
// then PUT don't accidentally blank out credentials they never saw.
//
// When no run is in flight: validate -> ApplySettings (hot-swap live
// components) -> Save to disk -> update in-memory Config. Any step can
// fail; failures after ApplySettings try to roll back by calling it
// again with the original config.
//
// When a run IS in flight: validate -> Save to disk -> stash as
// pendingConfig. The post-run goroutine drains pendingConfig and calls
// ApplySettings + updateConfig once the run finishes. This lets the
// operator queue a config change without waiting for the run to end.
// The response carries `pending_apply: true` so the UI can show that
// the save is queued rather than live.
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	live, ok := s.snapshotConfig()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}

	// Acquire applyMu BEFORE runMu so we serialise against the post-run
	// goroutine's apply: that goroutine takes runMu briefly to flip
	// currentRun=0 and then takes applyMu to drain pendingConfig. If we
	// took only runMu and saw currentRun==0, our save could land in the
	// gap between its runMu.Unlock and applyMu.Lock and then be reverted
	// by the queued apply. (#255)
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	// Read currentRun + pendingConfig under runMu briefly, then release
	// runMu so the slow ApplySettings (DNS, TCP handshake, HeadBucket)
	// and config.Save (fsync) don't block /status, /cancel, /stop. Since
	// handleTriggerRun also acquires applyMu before runMu (#233), no new
	// run can start while we're applying. (#229)
	s.runMu.Lock()
	currentRun := s.currentRun
	prev := live
	if s.pendingConfig != nil {
		prev = *s.pendingConfig
	}
	s.runMu.Unlock()

	var next config.Config
	if err := decodeJSON(r, &next); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	merged := mergeSecrets(next, prev)
	if err := merged.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if currentRun != 0 {
		// Defer the apply until the run finishes. Persist to disk now so
		// the change survives restart, but leave live state alone.
		if s.deps.SaveSettings != nil {
			if err := s.deps.SaveSettings(merged); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Errorf("save config: %w", err))
				return
			}
		} else if s.deps.ConfigPath != "" {
			if err := config.Save(s.deps.ConfigPath, merged); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Errorf("save config: %w", err))
				return
			}
		}
		queued := merged
		s.runMu.Lock()
		s.pendingConfig = &queued
		s.runMu.Unlock()
		writeJSON(w, http.StatusOK, settingsResponse{
			Config:       merged.Redacted(),
			PendingApply: true,
		})
		return
	}

	if s.deps.ApplySettings != nil {
		if err := s.deps.ApplySettings(live, merged); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("apply settings: %w", err))
			return
		}
	}

	if s.deps.SaveSettings != nil {
		if err := s.deps.SaveSettings(merged); err != nil {
			// Roll back the hot-swap so memory + components still match
			// what's on disk. If the rollback itself fails the live
			// process is in a half-swapped state (new src/store, old
			// config file): surface both errors so the operator knows
			// to restart and log the rollback failure.
			var rollbackErr error
			if s.deps.ApplySettings != nil {
				rollbackErr = s.deps.ApplySettings(merged, live)
			}
			s.setStoragePrefix(live.S3.KeyPrefix)
			if rollbackErr != nil {
				if s.deps.Logger != nil {
					s.deps.Logger.Error("settings rollback failed after save error",
						"save_err", err, "rollback_err", rollbackErr)
				}
				writeError(w, http.StatusInternalServerError,
					fmt.Errorf("save config: %w; rollback also failed: %v", err, rollbackErr))
				return
			}
			writeError(w, http.StatusInternalServerError, fmt.Errorf("save config: %w", err))
			return
		}
	} else if s.deps.ConfigPath != "" {
		if err := config.Save(s.deps.ConfigPath, merged); err != nil {
			var rollbackErr error
			if s.deps.ApplySettings != nil {
				rollbackErr = s.deps.ApplySettings(merged, live)
			}
			s.setStoragePrefix(live.S3.KeyPrefix)
			if rollbackErr != nil {
				if s.deps.Logger != nil {
					s.deps.Logger.Error("settings rollback failed after save error",
						"save_err", err, "rollback_err", rollbackErr)
				}
				writeError(w, http.StatusInternalServerError,
					fmt.Errorf("save config: %w; rollback also failed: %v", err, rollbackErr))
				return
			}
			writeError(w, http.StatusInternalServerError, fmt.Errorf("save config: %w", err))
			return
		}
	}

	s.updateConfig(merged)
	// No run in flight, so a pending config (if any was somehow set) is
	// stale — clear it so the UI doesn't keep flagging pending_apply.
	s.runMu.Lock()
	s.pendingConfig = nil
	s.runMu.Unlock()
	writeJSON(w, http.StatusOK, settingsResponse{
		Config:       merged.Redacted(),
		PendingApply: false,
	})
}

// applyPendingSettings runs ApplySettings + updateConfig for a config
// that was queued during an in-flight run. Called from the post-run
// goroutine after currentRun has been cleared, so no engine still holds
// the old source/storage references that ApplySettings will close.
//
// On apply failure we keep the disk state (the operator's intent) but
// leave live state on the previous config and clear pending — the disk
// vs live divergence is the agreed failure mode (a follow-up PUT, now
// allowed since no run is in flight, or a process restart will resolve
// it).
func (s *Server) applyPendingSettings(pending *config.Config, logger *slog.Logger) {
	if pending == nil {
		return
	}
	// Skip the apply when shutdown has already started: ApplySettings
	// builds new *S3Storage / *SMB clients with context.Background(),
	// which can hang on the SDK's default dial timeout while pointing
	// at the new (potentially unreachable) endpoint — and runServe is
	// about to tear those clients right back down via app.close().
	// The persisted config is reapplied on next start. (#180)
	if s.shutdownCh != nil {
		select {
		case <-s.shutdownCh:
			if logger != nil {
				logger.Info("pending settings deferred — shutdown in progress; will apply on next start")
			}
			return
		default:
		}
	}
	live, ok := s.snapshotConfig()
	if !ok {
		return
	}
	if s.deps.ApplySettings != nil {
		if err := s.deps.ApplySettings(live, *pending); err != nil {
			if logger != nil {
				logger.Error("apply pending settings failed", "err", err)
			}
			return
		}
	}
	s.updateConfig(*pending)
	if logger != nil {
		logger.Info("pending settings applied after run")
	}
}

func mergeSecrets(next, existing config.Config) config.Config {
	if next.Source.SMB.Password == config.RedactedMarker {
		next.Source.SMB.Password = existing.Source.SMB.Password
	}
	if next.S3.AccessKeyID == config.RedactedMarker {
		next.S3.AccessKeyID = existing.S3.AccessKeyID
	}
	if next.S3.SecretAccessKey == config.RedactedMarker {
		next.S3.SecretAccessKey = existing.S3.SecretAccessKey
	}
	return next
}
