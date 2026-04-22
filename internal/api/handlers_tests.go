package api

import (
	"errors"
	"net/http"
	"os"

	"github.com/Wlczak/aws-backup/internal/config"
)

type testResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// handleTestSource verifies the configured source is reachable. For
// localdir that means the root exists and is a directory; the real SMB
// impl plugs in its own check in feature 18.
func (s *Server) handleTestSource(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Config == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	switch s.deps.Config.Source.Type {
	case config.SourceLocalDir:
		root := s.deps.Config.Source.LocalDir.Root
		st, err := os.Stat(root)
		if err != nil {
			writeJSON(w, http.StatusOK, testResult{OK: false, Message: err.Error()})
			return
		}
		if !st.IsDir() {
			writeJSON(w, http.StatusOK, testResult{OK: false, Message: root + " is not a directory"})
			return
		}
		writeJSON(w, http.StatusOK, testResult{OK: true, Message: "localdir root reachable"})
	case config.SourceSMB:
		writeJSON(w, http.StatusOK, testResult{OK: false, Message: "SMB source adapter not wired yet (feature 18)"})
	default:
		writeJSON(w, http.StatusOK, testResult{OK: false, Message: "unknown source type"})
	}
}

// handleTestStorage is a stub until feature 19 — it returns ok for MinIO
// endpoints (the ones aws-backup is configured against today) and
// otherwise reports "not yet wired" to avoid accidentally reaching real AWS.
func (s *Server) handleTestStorage(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Config == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	endpoint := s.deps.Config.S3.Endpoint
	if endpoint == "" {
		writeJSON(w, http.StatusOK, testResult{
			OK:      false,
			Message: "real AWS S3 is gated until feature 19; set s3.endpoint to MinIO to test",
		})
		return
	}
	writeJSON(w, http.StatusOK, testResult{
		OK:      true,
		Message: "using custom endpoint: " + endpoint,
	})
}
