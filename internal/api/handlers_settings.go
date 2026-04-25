package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Wlczak/aws-backup/internal/config"
)

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Config == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Config.Redacted())
}

// handlePutSettings accepts a full Config in the body. Fields equal to
// RedactedMarker ("***") preserve the existing value so clients that GET
// then PUT don't accidentally blank out credentials they never saw.
//
// Ordering: validate -> ApplySettings (hot-swap live components) ->
// Save to disk -> update in-memory Config. Any step can fail; failures
// after ApplySettings try to roll back by calling it again with the
// original config.
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	if s.deps.Config == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	var next config.Config
	if err := decodeJSON(r, &next); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	prev := *s.deps.Config
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
			// what's on disk.
			if s.deps.ApplySettings != nil {
				_ = s.deps.ApplySettings(merged, prev)
			}
			s.deps.StoragePrefix = prev.S3.KeyPrefix
			writeError(w, http.StatusInternalServerError, fmt.Errorf("save config: %w", err))
			return
		}
	}

	*s.deps.Config = merged
	s.deps.StoragePrefix = merged.S3.KeyPrefix
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
