package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/storage"
)

// shouldUseResumable reports whether the engine's storage backend
// supports the resumable multipart path AND size meets the configured
// threshold. MemStorage and other test fakes that don't implement
// storage.ResumableStorage transparently fall through to the existing
// single-shot Put path.
func (e *Engine) shouldUseResumable(size int64) (*storage.S3Storage, int64, bool) {
	rs, ok := e.opts.Storage.(*storage.S3Storage)
	if !ok || rs == nil {
		return nil, 0, false
	}
	threshold := rs.ResumeThreshold()
	if threshold <= 0 || size < threshold {
		return nil, 0, false
	}
	return rs, rs.PartSize(), true
}

// resumePutOpts threads through the small slice of state the engine
// needs the resume path to know about. Exactly one of FileID / ZipKey
// is populated.
type resumePutOpts struct {
	FileID  int64
	ZipKey  string
	Class   s3types.StorageClass
	IfAbsent bool // mirror PutIfAbsent semantics for zips
}

// putResumable drives one resumable upload end-to-end:
//
//  1. Look up any persisted multipart-upload row.
//  2. If one exists but its content_sha256 disagrees with the local
//     tmp's hash, abort the old upload on S3, drop the row, and
//     start fresh. (Per the design doc, this is the "tightened
//     scope" the operator picked: never resume against a tmp that
//     might have changed.)
//  3. If IfAbsent is requested and there's no in-flight row, HEAD
//     the key first; an existing object becomes ErrAlreadyExists so
//     the engine's zip-collision retry path keeps working.
//  4. Call PutResumable, persisting the UploadId synchronously
//     before the first part lands so a crash before any UploadPart
//     still leaves a recoverable record.
//  5. On success delete the row; on error keep it so the next run
//     can resume.
func (e *Engine) putResumable(ctx context.Context, runID int64, key, tmpPath string, size int64, contentSHA256hex string, opts resumePutOpts) (storage.PutResult, error) {
	rs, partSize, ok := e.shouldUseResumable(size)
	if !ok {
		return storage.PutResult{}, errors.New("engine: putResumable called without resumable storage")
	}

	// 1. Look up persisted state.
	var (
		state *db.MultipartUpload
		err   error
	)
	if opts.FileID > 0 {
		state, err = e.opts.DB.GetMultipartUploadByFile(ctx, opts.FileID)
	} else if opts.ZipKey != "" {
		state, err = e.opts.DB.GetMultipartUploadByZipKey(ctx, opts.ZipKey)
	}
	if err != nil {
		return storage.PutResult{}, fmt.Errorf("load multipart state: %w", err)
	}

	// 2. Stale-tmp check: if the local file no longer matches the
	// content we promised to upload, abort the old upload and start
	// fresh.
	if state != nil && state.ContentSHA256 != contentSHA256hex {
		_ = rs.AbortMultipart(ctx, storage.MultipartUpload{
			Key: state.S3Key, UploadID: state.UploadID, PartSize: state.PartSize,
		})
		if delErr := e.deleteMultipartRow(ctx, opts); delErr != nil {
			return storage.PutResult{}, fmt.Errorf("clear stale multipart row: %w", delErr)
		}
		state = nil
	}

	// 3. IfAbsent: only check when there's no in-flight resume — the
	// presence of an UploadId means we already committed to writing
	// at this key.
	if opts.IfAbsent && state == nil {
		if _, hErr := rs.Head(ctx, key); hErr == nil {
			return storage.PutResult{}, storage.ErrAlreadyExists
		} else if !errors.Is(hErr, storage.ErrNotFound) {
			return storage.PutResult{}, hErr
		}
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		return storage.PutResult{}, err
	}
	defer f.Close()

	// 4. Drive PutResumable. partSize is locked to whatever the
	// previous run used, so an operator who lowers PartSize between
	// runs doesn't accidentally invalidate already-uploaded parts.
	useUploadID := ""
	usePartSize := partSize
	if state != nil {
		useUploadID = state.UploadID
		usePartSize = state.PartSize
	}

	rOpts := storage.ResumableOptions{
		UploadID:     useUploadID,
		PartSize:     usePartSize,
		Parallel:     e.opts.UploadThreads,
		StorageClass: opts.Class,
		OnUploadID: func(uploadID string) error {
			row := db.MultipartUpload{
				FileID:        opts.FileID,
				ZipKey:        opts.ZipKey,
				S3Key:         key,
				UploadID:      uploadID,
				PartSize:      usePartSize,
				Size:          size,
				ContentSHA256: contentSHA256hex,
				StartedAt:     time.Now().UTC(),
			}
			return e.opts.DB.UpsertMultipartUpload(ctx, row)
		},
		OnProgress: func() func(bytes, total int64) {
			// Throttle to defaultProgressInterval so a fast-network
			// multipart doesn't flood the SSE bus with per-part events
			// — same cadence the single-shot path gets via
			// uploadProgressCtx. (#167)
			var lastEmit time.Time
			return func(bytes, total int64) {
				now := e.opts.Now()
				if !lastEmit.IsZero() && bytes < total && now.Sub(lastEmit) < defaultProgressInterval {
					return
				}
				lastEmit = now
				percent := 0.0
				if total > 0 {
					percent = float64(bytes) / float64(total) * 100
				}
				e.emit(Event{
					Type:  EventUploadProgress,
					RunID: runID,
					At:    now,
					Data: map[string]any{
						"key":            key,
						"size":           total,
						"bytes_uploaded": bytes,
						"percent":        percent,
					},
				})
			}
		}(),
	}

	res, _, err := rs.PutResumable(ctx, key, f, size, rOpts)
	if err != nil {
		// On checksum mismatch the underlying call already aborted
		// the upload — surface as a fresh-restart hint so the engine
		// retries this object next run from byte 0.
		if errors.Is(err, storage.ErrChecksumMismatch) {
			_ = e.deleteMultipartRow(ctx, opts)
		}
		return storage.PutResult{}, err
	}

	// 5. Success — drop the persisted row.
	if delErr := e.deleteMultipartRow(ctx, opts); delErr != nil {
		// Non-fatal: the upload completed successfully. Worst case
		// the row is reaped by the optional stale-rows sweep, or by
		// the next attempt's mismatch path.
		slog.Warn("delete multipart row after success", "key", key, "err", delErr)
	}
	return res, nil
}

// deleteMultipartRow drops the in-flight row keyed on whichever
// discriminator the caller populated.
func (e *Engine) deleteMultipartRow(ctx context.Context, opts resumePutOpts) error {
	if opts.FileID > 0 {
		return e.opts.DB.DeleteMultipartUploadByFile(ctx, opts.FileID)
	}
	if opts.ZipKey != "" {
		return e.opts.DB.DeleteMultipartUploadByZipKey(ctx, opts.ZipKey)
	}
	return nil
}
