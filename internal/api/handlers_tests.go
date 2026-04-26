package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/source"
	"github.com/Wlczak/aws-backup/internal/storage"
)

type testResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// handleTestSource verifies the configured source is reachable. For
// localdir that means the root exists and is a directory; the real SMB
// impl plugs in its own check in feature 18.
func (s *Server) handleTestSource(w http.ResponseWriter, _ *http.Request) {
	cfg, ok := s.snapshotConfig()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}
	switch cfg.Source.Type {
	case config.SourceLocalDir:
		root := cfg.Source.LocalDir.Root
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
		smb, err := source.FromConfig(cfg.Source)
		if err != nil {
			writeJSON(w, http.StatusOK, testResult{OK: false, Message: err.Error()})
			return
		}
		_ = smb.Close()
		writeJSON(w, http.StatusOK, testResult{OK: true, Message: "smb share reachable"})
	default:
		writeJSON(w, http.StatusOK, testResult{OK: false, Message: "unknown source type"})
	}
}

// handleTestStorage builds a transient S3 client from the live config and
// HEADs the configured bucket. Works against real AWS (endpoint empty,
// SDK default credential chain) and any S3-compatible endpoint (MinIO).
// SDK errors are surfaced verbatim so the user sees NotFound,
// InvalidAccessKeyId, dial errors, etc.
func (s *Server) handleTestStorage(w http.ResponseWriter, r *http.Request) {
	cfg, ok := s.snapshotConfig()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("config not loaded"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	client, err := storage.NewS3Storage(ctx, storage.S3Config{
		Endpoint:        cfg.S3.Endpoint,
		UsePathStyle:    cfg.S3.UsePathStyle,
		Region:          cfg.S3.Region,
		Bucket:          cfg.S3.Bucket,
		AccessKeyID:     cfg.S3.AccessKeyID,
		SecretAccessKey: cfg.S3.SecretAccessKey,
		StorageClass:    cfg.S3.StorageClass,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, testResult{OK: false, Message: err.Error()})
		return
	}
	defer client.Close()

	if err := client.HeadBucket(ctx); err != nil {
		writeJSON(w, http.StatusOK, testResult{OK: false, Message: err.Error()})
		return
	}

	target := "AWS S3"
	if cfg.S3.Endpoint != "" {
		target = cfg.S3.Endpoint
	}
	writeJSON(w, http.StatusOK, testResult{
		OK:      true,
		Message: "bucket reachable: " + cfg.S3.Bucket + " (" + target + ")",
	})
}
