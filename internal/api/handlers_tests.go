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
func (s *Server) handleTestSource(w http.ResponseWriter, r *http.Request) {
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
		// Bound the dial: source.FromConfig takes no ctx and the OS
		// default TCP-connect timeout (~75–120 s) would otherwise pin a
		// goroutine + FD per request against a black-holed host. (#177)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		type result struct {
			smb source.Source
			err error
		}
		ch := make(chan result, 1)
		go func() {
			smb, err := source.FromConfig(cfg.Source)
			ch <- result{smb, err}
		}()
		select {
		case res := <-ch:
			if res.err != nil {
				writeJSON(w, http.StatusOK, testResult{OK: false, Message: res.err.Error()})
				return
			}
			_ = res.smb.Close()
			writeJSON(w, http.StatusOK, testResult{OK: true, Message: "smb share reachable"})
		case <-ctx.Done():
			writeJSON(w, http.StatusOK, testResult{OK: false, Message: "smb dial timed out after 10s"})
			// Don't leak the goroutine OR the SMB connection it may yet
			// establish: drain the late result and close any successful
			// dial so we don't pile up FDs + sessions per timed-out test
			// against a slow share. (#267)
			go func() {
				res := <-ch
				if res.err == nil && res.smb != nil {
					_ = res.smb.Close()
				}
			}()
		}
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
