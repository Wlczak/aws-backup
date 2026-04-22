package api

import (
	"fmt"
	"net/http"
)

type syncResponse struct {
	KeysInDB     int   `json:"keys_in_db"`
	KeysInS3     int   `json:"keys_in_s3"`
	MissingInS3  int   `json:"missing_in_s3"`
	FilesReset   int64 `json:"files_reset"`
}

// handleSync lists all s3_keys from the DB and all keys from S3, finds
// keys that are recorded as uploaded but absent from S3, and resets those
// DB rows to pending so the next upload run will re-upload them.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if s.deps.Storage == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("storage not configured"))
		return
	}

	dbKeys, err := s.deps.DB.ListS3Keys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("list db keys: %w", err))
		return
	}

	s3Keys, err := s.deps.Storage.List(r.Context(), s.deps.StoragePrefix)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("list s3 keys: %w", err))
		return
	}

	inS3 := make(map[string]struct{}, len(s3Keys))
	for _, k := range s3Keys {
		inS3[k] = struct{}{}
	}

	var missing []string
	for _, k := range dbKeys {
		if _, ok := inS3[k]; !ok {
			missing = append(missing, k)
		}
	}

	var reset int64
	if len(missing) > 0 {
		reset, err = s.deps.DB.MarkPendingByS3Keys(r.Context(), missing)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("reset missing: %w", err))
			return
		}
	}

	writeJSON(w, http.StatusOK, syncResponse{
		KeysInDB:    len(dbKeys),
		KeysInS3:    len(s3Keys),
		MissingInS3: len(missing),
		FilesReset:  reset,
	})
}
