package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Wlczak/aws-backup/internal/config"
)

// handleSetupComplete is the final server-side gate for onboarding. It does
// not trust prior browser tests: both configured resources are probed again
// before the durable completion flag is written.
func (s *Server) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	central, err := config.LoadCentral(s.deps.CentralConfigPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !central.SetupRequired() {
		writeJSON(w, http.StatusOK, authStatusResponse{
			PasswordSet:   central.Auth.PasswordHash != "",
			Authenticated: true,
			SetupRequired: false,
		})
		return
	}

	cfg, ok := s.snapshotConfig()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	if err := cfg.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if cfg.S3.Bucket == "" {
		writeError(w, http.StatusBadRequest, errors.New("s3 bucket is required to complete setup"))
		return
	}
	if s.deps.ValidateSetup != nil {
		if err := s.deps.ValidateSetup(r.Context(), cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	} else {
		if result := testSourceConfig(r.Context(), cfg); !result.OK {
			writeError(w, http.StatusBadRequest, fmt.Errorf("source test failed: %s", result.Message))
			return
		}
		if result := testStorageConfig(r.Context(), cfg); !result.OK {
			writeError(w, http.StatusBadRequest, fmt.Errorf("storage test failed: %s", result.Message))
			return
		}
	}

	central.MarkSetupCompleted()
	if err := config.SaveCentral(s.deps.CentralConfigPath, central); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("save setup state: %w", err))
		return
	}
	if s.deps.SetupCompleted != nil {
		s.deps.SetupCompleted()
	}
	writeJSON(w, http.StatusOK, authStatusResponse{
		PasswordSet:   true,
		Authenticated: true,
		SetupRequired: false,
	})
}
