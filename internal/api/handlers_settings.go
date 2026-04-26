package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Wlczak/aws-backup/internal/config"
)

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	cfg, ok := s.snapshotConfig()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	writeJSON(w, http.StatusOK, cfg.Redacted())
}

// handlePutSettings accepts a full Config in the body. Fields equal to
// RedactedMarker ("***") preserve the existing value so clients that GET
// then PUT don't accidentally blank out credentials they never saw.
//
// Ordering: validate -> ApplySettings (hot-swap live components) ->
// Save to disk -> update in-memory Config. Any step can fail; failures
// after ApplySettings try to roll back by calling it again with the
// original config.
//
// Refuses to run while a backup is in progress: the hot-swap closes the
// previously-active Source/Storage handles, which would break a run
// holding references to them. 409 the caller and let them retry.
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	prev, ok := s.snapshotConfig()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}

	s.runMu.Lock()
	if s.currentRun != 0 {
		current := s.currentRun
		s.runMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":          "cannot change settings while a backup run is in progress",
			"current_run_id": current,
		})
		return
	}
	defer s.runMu.Unlock()

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

	if s.deps.ApplySettings != nil {
		if err := s.deps.ApplySettings(prev, merged); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("apply settings: %w", err))
			return
		}
	}

	if s.deps.ConfigPath != "" {
		if err := config.Save(s.deps.ConfigPath, merged); err != nil {
			// Roll back the hot-swap so memory + components still match
			// what's on disk. If the rollback itself fails the live
			// process is in a half-swapped state (new src/store, old
			// config file): surface both errors so the operator knows
			// to restart and log the rollback failure.
			var rollbackErr error
			if s.deps.ApplySettings != nil {
				rollbackErr = s.deps.ApplySettings(merged, prev)
			}
			s.setStoragePrefix(prev.S3.KeyPrefix)
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
	writeJSON(w, http.StatusOK, merged.Redacted())
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
