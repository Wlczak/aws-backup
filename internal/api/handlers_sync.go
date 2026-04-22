package api

import (
	"fmt"
	"net/http"
	"strings"
)

type syncResponse struct {
	ZipNamesInDB      int   `json:"zip_names_in_db"`
	IndividualKeysInDB int  `json:"individual_keys_in_db"`
	KeysInS3          int   `json:"keys_in_s3"`
	MissingZips       int   `json:"missing_zips"`
	MissingIndividual int   `json:"missing_individual"`
	FilesReset        int64 `json:"files_reset"`
}

// handleSync checks that every recorded S3 object still exists in the bucket.
// Zipped files are matched by zip_name; individually-uploaded files by s3_key.
// Missing objects are reset to pending so the next run re-uploads them.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if s.deps.Storage == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("storage not configured"))
		return
	}
	ctx := r.Context()

	zipNames, err := s.deps.DB.ListZipNames(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("list zip names: %w", err))
		return
	}

	indivKeys, err := s.deps.DB.ListIndividualS3Keys(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("list individual keys: %w", err))
		return
	}

	s3Keys, err := s.deps.Storage.List(ctx, s.deps.StoragePrefix)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("list s3 keys: %w", err))
		return
	}

	inS3 := make(map[string]struct{}, len(s3Keys))
	for _, k := range s3Keys {
		inS3[k] = struct{}{}
	}

	// Check zip objects: the S3 key for a zip is prefix + zipName.
	var missingZips []string
	for _, z := range zipNames {
		key := joinKey(s.deps.StoragePrefix, z)
		if _, ok := inS3[key]; !ok {
			missingZips = append(missingZips, z)
		}
	}

	// Check individually-uploaded files: s3_key is already the full S3 key.
	var missingIndiv []string
	for _, k := range indivKeys {
		if _, ok := inS3[k]; !ok {
			missingIndiv = append(missingIndiv, k)
		}
	}

	var reset int64
	if len(missingZips) > 0 {
		n, err := s.deps.DB.MarkPendingByZipNames(ctx, missingZips)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("reset missing zips: %w", err))
			return
		}
		reset += n
	}
	if len(missingIndiv) > 0 {
		n, err := s.deps.DB.MarkPendingByS3Keys(ctx, missingIndiv)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("reset missing individual: %w", err))
			return
		}
		reset += n
	}

	writeJSON(w, http.StatusOK, syncResponse{
		ZipNamesInDB:       len(zipNames),
		IndividualKeysInDB: len(indivKeys),
		KeysInS3:           len(s3Keys),
		MissingZips:        len(missingZips),
		MissingIndividual:  len(missingIndiv),
		FilesReset:         reset,
	})
}

// joinKey concatenates a prefix and a name with a single "/" separator,
// handling the case where prefix is empty or already ends with "/".
func joinKey(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return strings.TrimRight(prefix, "/") + "/" + name
}
