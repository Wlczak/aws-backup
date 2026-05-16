package engine

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/pathutil"
	"github.com/Wlczak/aws-backup/internal/storage"
)

// DownloadOptions configures the dashboard-triggered full download job.
// It mirrors the bucket-backed index into a local directory and only
// re-downloads rows that are still missing from that mirror.
type DownloadOptions struct {
	DB          *db.DB
	Storage     storage.Storage
	KeyPrefix   string
	DownloadDir string
	TmpDir      string
	Emit        EventEmitter
}

// DownloadStats summarizes a mirror sync + download pass.
type DownloadStats struct {
	Scanned      int64
	Present      int64
	Missing      int64
	ObjectCount  int64
	FilesWritten int64
	BytesWritten int64
	TotalBytes   int64
	Skipped      []string
	Errors       []string
}

// DownloadMirrorToDir scans the configured download folder, updates the
// mirror columns in the DB, and then downloads only rows still missing
// from that folder.
func DownloadMirrorToDir(ctx context.Context, opts DownloadOptions) (DownloadStats, error) {
	var stats DownloadStats
	if opts.DB == nil {
		return stats, errors.New("download: DB is required")
	}
	if opts.Storage == nil {
		return stats, errors.New("download: Storage is required")
	}
	if opts.DownloadDir == "" {
		return stats, errors.New("download: DownloadDir is required")
	}
	if !filepath.IsAbs(opts.DownloadDir) {
		return stats, fmt.Errorf("download: DownloadDir must be absolute (got %q)", opts.DownloadDir)
	}
	if opts.TmpDir == "" {
		opts.TmpDir = os.TempDir()
	}
	if err := os.MkdirAll(opts.DownloadDir, 0o755); err != nil {
		return stats, fmt.Errorf("mkdir download dir: %w", err)
	}
	if err := os.MkdirAll(opts.TmpDir, 0o755); err != nil {
		return stats, fmt.Errorf("mkdir tmp dir: %w", err)
	}

	total, err := countDownloadCandidates(ctx, opts.DB)
	if err != nil {
		return stats, err
	}

	emit := opts.Emit
	if emit != nil {
		emit(Event{
			Type: EventDownloadMirrorScanStart,
			At:   time.Now(),
			Data: map[string]any{
				"total": total,
			},
		})
	}

	presentByID := make([]int64, 0, 256)
	missingByID := make([]int64, 0, 256)
	recoverableMissing := make([]db.File, 0, 256)
	const pageSize = 1000
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		rows, _, err := opts.DB.ListFiles(ctx, db.FilesFilter{Page: page, Limit: pageSize})
		if err != nil {
			return stats, fmt.Errorf("list files: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, f := range rows {
			stats.Scanned++
			present, err := mirrorFilePresent(opts.DownloadDir, f)
			if err != nil {
				stats.Errors = append(stats.Errors, f.Path+": "+err.Error())
				missingByID = append(missingByID, f.ID)
				stats.Missing++
				if recoverableDownloadRow(f) {
					recoverableMissing = append(recoverableMissing, f)
				}
				continue
			}
			if present {
				stats.Present++
				presentByID = append(presentByID, f.ID)
			} else {
				stats.Missing++
				missingByID = append(missingByID, f.ID)
				if recoverableDownloadRow(f) {
					recoverableMissing = append(recoverableMissing, f)
				}
			}
		}
		if err := opts.DB.MarkDownloadMirrorBatch(ctx, presentByID, missingByID, time.Now().UTC()); err != nil {
			return stats, err
		}
		presentByID = presentByID[:0]
		missingByID = missingByID[:0]
		if emit != nil {
			emit(Event{
				Type: EventDownloadMirrorScanProgress,
				At:   time.Now(),
				Data: map[string]any{
					"scanned": stats.Scanned,
					"present": stats.Present,
					"missing": stats.Missing,
					"total":   total,
				},
			})
		}
		if len(rows) < pageSize {
			break
		}
	}
	byZip := map[string][]db.File{}
	standalone := make([]db.File, 0, len(recoverableMissing))
	for _, f := range recoverableMissing {
		if f.ZipName != "" {
			key := f.S3Key
			if key == "" {
				key = pathutil.JoinKey(opts.KeyPrefix, f.ZipName)
			}
			byZip[key] = append(byZip[key], f)
			continue
		}
		standalone = append(standalone, f)
	}
	sort.Slice(standalone, func(i, j int) bool { return standalone[i].Path < standalone[j].Path })

	downloadTotal := int64(len(standalone))
	for _, members := range byZip {
		downloadTotal += int64(len(members))
	}
	downloadObjectCount := int64(len(standalone) + len(byZip))
	downloadBytesTotal := int64(0)
	for _, f := range standalone {
		downloadBytesTotal += f.Size
	}
	for _, members := range byZip {
		for _, f := range members {
			downloadBytesTotal += f.Size
		}
	}
	if emit != nil {
		emit(Event{
			Type: EventDownloadMirrorScanComplete,
			At:   time.Now(),
			Data: map[string]any{
				"scanned":      stats.Scanned,
				"present":      stats.Present,
				"missing":      stats.Missing,
				"total":        total,
				"total_bytes":  downloadBytesTotal,
				"object_count": downloadObjectCount,
			},
		})
	}
	if len(recoverableMissing) == 0 {
		return stats, nil
	}
	if emit != nil {
		emit(Event{
			Type: EventDownloadMirrorStart,
			At:   time.Now(),
			Data: map[string]any{
				"total":        downloadTotal,
				"total_bytes":  downloadBytesTotal,
				"object_count": downloadObjectCount,
			},
		})
	}

	processed := int64(0)
	for _, f := range standalone {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		n, err := downloadStandaloneMirror(ctx, opts, f)
		processed++
		if err != nil {
			stats.Errors = append(stats.Errors, f.Path+": "+err.Error())
			if emit != nil {
				emit(Event{
					Type: EventDownloadMirrorProgress,
					At:   time.Now(),
					Data: map[string]any{
						"path":          f.Path,
						"processed":     processed,
						"total":         downloadTotal,
						"total_bytes":   downloadBytesTotal,
						"files_written": stats.FilesWritten,
						"bytes_written": stats.BytesWritten,
						"errors":        len(stats.Errors),
						"error":         err.Error(),
					},
				})
			}
			continue
		}
		stats.FilesWritten++
		stats.BytesWritten += n
		if err := opts.DB.MarkDownloadMirrorBatch(ctx, []int64{f.ID}, nil, time.Now().UTC()); err != nil {
			return stats, err
		}
		if emit != nil {
			emit(Event{
				Type: EventDownloadMirrorProgress,
				At:   time.Now(),
				Data: map[string]any{
					"path":          f.Path,
					"processed":     processed,
					"total":         downloadTotal,
					"total_bytes":   downloadBytesTotal,
					"files_written": stats.FilesWritten,
					"bytes_written": stats.BytesWritten,
					"errors":        len(stats.Errors),
				},
			})
		}
	}

	zipKeys := make([]string, 0, len(byZip))
	for k := range byZip {
		zipKeys = append(zipKeys, k)
	}
	sort.Strings(zipKeys)
	for _, key := range zipKeys {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		members := byZip[key]
		membersWritten, bytesWritten, errs, nextProcessed := downloadZipMembersMirror(ctx, opts, key, members, processed, downloadTotal, downloadBytesTotal, emit)
		processed = nextProcessed
		stats.FilesWritten += membersWritten
		stats.BytesWritten += bytesWritten
		stats.Errors = append(stats.Errors, errs...)
	}

	if emit != nil {
		emit(Event{
			Type: EventDownloadMirrorComplete,
			At:   time.Now(),
			Data: map[string]any{
				"files_written": stats.FilesWritten,
				"bytes_written": stats.BytesWritten,
				"total_bytes":   downloadBytesTotal,
				"object_count":  downloadObjectCount,
				"errors":        len(stats.Errors),
			},
		})
	}
	stats.ObjectCount = downloadObjectCount
	stats.TotalBytes = downloadBytesTotal
	return stats, nil
}

func countDownloadCandidates(ctx context.Context, d *db.DB) (int64, error) {
	_, total, err := d.ListFiles(ctx, db.FilesFilter{Page: 1, Limit: 1})
	return total, err
}

func mirrorFilePresent(root string, f db.File) (bool, error) {
	dst, err := safeJoin(root, f.Path)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.IsDir() {
		return false, nil
	}
	return info.Size() == f.Size, nil
}

func recoverableDownloadRow(f db.File) bool {
	switch f.Status {
	case db.StatusUploaded, db.StatusZipped, db.StatusCloudOnly:
		return f.S3Key != "" || f.ZipName != ""
	default:
		return false
	}
}

func downloadStandaloneMirror(ctx context.Context, opts DownloadOptions, f db.File) (int64, error) {
	return restoreStandalone(ctx, opts.Storage, opts.DownloadDir, f, f.MD5, nil)
}

func downloadZipMembersMirror(
	ctx context.Context,
	opts DownloadOptions,
	key string,
	members []db.File,
	processed, total int64,
	totalBytes int64,
	emit EventEmitter,
) (written, bytes int64, errs []string, nextProcessed int64) {
	zipPath := mirrorZipCachePath(opts.TmpDir, key)
	if err := ensureZipCache(ctx, opts.Storage, key, zipPath); err != nil {
		if errors.Is(err, storage.ErrGlacierThawing) {
			errs = append(errs, key+": still thawing from glacier: "+err.Error())
		} else {
			errs = append(errs, key+": download zip: "+err.Error())
		}
		return 0, 0, errs, processed
	}
	w, b, innerErrs, next := extractZipMembers(ctx, opts.DownloadDir, key, zipPath, members, int(processed), int(total), totalBytes, emit, false, func(f db.File) error {
		return opts.DB.MarkDownloadMirrorBatch(ctx, []int64{f.ID}, nil, time.Now().UTC())
	})
	errs = append(errs, innerErrs...)
	return w, b, errs, int64(next)
}

func mirrorZipCachePath(tmpDir, key string) string {
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:]) + ".zip"
	return filepath.Join(tmpDir, "download-cache", name)
}

func ensureZipCache(ctx context.Context, s storage.Storage, key, cachePath string) error {
	if zr, err := zip.OpenReader(cachePath); err == nil {
		zr.Close()
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(cachePath)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}
	if err := downloadZipCache(ctx, s, key, cachePath); err != nil {
		return err
	}
	return nil
}

func downloadZipCache(ctx context.Context, s storage.Storage, key, cachePath string) error {
	tmp, err := os.CreateTemp(filepath.Dir(cachePath), filepath.Base(cachePath)+".*.part")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	rc, err := s.Get(ctx, key)
	if err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, rc); err != nil {
		rc.Close()
		tmp.Close()
		return err
	}
	rc.Close()
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, cachePath); err != nil {
		return err
	}
	return nil
}
