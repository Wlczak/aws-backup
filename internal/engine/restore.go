package engine

import (
	"archive/zip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/pathutil"
	"github.com/Wlczak/aws-backup/internal/storage"
)

// RestoreOptions configures a single RestoreToDir call. Everything it
// needs is supplied per-call so restore can run without a full Engine
// wired to a source/scheduler.
type RestoreOptions struct {
	DB        *db.DB
	Storage   storage.Storage
	KeyPrefix string
	// TargetDir is the absolute local directory files are written into.
	// Source-relative paths are preserved beneath it, so a DB row at
	// "photos/2024/a.jpg" writes to <TargetDir>/photos/2024/a.jpg.
	TargetDir string
	// Paths selects which files to restore by prefix match — an entry
	// "photos" selects every file under "photos/". "/" or "" selects
	// everything. Unknown paths come back in RestoreStats.Skipped.
	Paths []string
	// TmpDir is where downloaded zip archives are staged before extraction.
	// Empty falls back to os.TempDir(); set this to the configured backup
	// TmpDir so multi-GB restores don't exhaust a small /tmp tmpfs. (#74)
	TmpDir string
	// SkipChecksum disables the post-write MD5 check against the DB row.
	// Off by default — verification is essentially free (hashing happens
	// in flight via io.MultiWriter) and protects against bit-rot in S3,
	// a tampered endpoint, or local-disk corruption that S3's transit
	// SHA256 wouldn't catch. Opt out only for huge restores where the
	// extra hashing visibly hurts. (#75)
	SkipChecksum bool
	// Emit, when non-nil, publishes restore_download_* progress events
	// while RestoreToDir is downloading and verifying files. The API
	// handler wires this to the in-process bus so the Restore page can
	// render a live progress bar.
	Emit EventEmitter
}

// RestoreStats summarizes the outcome of RestoreToDir. Errors are
// returned per-file so one bad object doesn't abort a bulk restore.
type RestoreStats struct {
	FilesWritten int64
	BytesWritten int64
	Skipped      []string
	Errors       []string
}

// RestoreRequestOptions configures a RequestRestore call. Only operates
// on the bucket — no local writes, no downloads. Use this to ask S3 to
// thaw archived objects out into the standard tier so they're available
// for download (or later inspection) for `Days` days.
type RestoreRequestOptions struct {
	DB        *db.DB
	Storage   storage.Storage
	KeyPrefix string
	// Tier selects the Glacier retrieval speed/cost tradeoff used for
	// the restore request. Empty defaults to Standard so older callers
	// keep their previous behavior unless they opt in explicitly.
	Tier storage.RestoreTier
	// Paths selects which DB rows to thaw by prefix match. "" or "/"
	// means "every uploaded row". Unknown paths land in
	// RestoreRequestStats.UnknownPaths.
	Paths []string
	// Days bounds how long the restored copy stays in the standard tier
	// before reverting. 1..30 is the operator-friendly range; AWS S3
	// accepts 1..N where N depends on the retrieval tier. Validated by
	// the caller (the API handler clamps to [1, 30]).
	Days int
	// Emit is an optional progress sink. When set, RequestRestore publishes
	// restore_request_start / _progress / _complete / _failed events so the
	// UI can render a live progress bar — issuing one s3:RestoreObject per
	// archive key takes wall-clock seconds at scale, and without progress
	// the operator has no way to tell if anything is happening.
	Emit EventEmitter
}

// restoreRequestEmitEvery throttles the cadence of restore_request_progress
// events. We still emit on the first key and the last key regardless, so a
// small restore (≤ progressEvery keys) still gets a visible signal.
const restoreRequestEmitEvery = 10

// RestoreRequestStats summarizes the outcome of RequestRestore.
type RestoreRequestStats struct {
	// KeysRequested is the number of distinct S3 keys (zip archives +
	// standalone objects) for which a fresh restore was issued.
	KeysRequested int
	// KeysAlreadyInProgress is the number of keys S3 reported as already
	// thawing (RestoreAlreadyInProgress). The caller treats this as a
	// soft-success — there's nothing more to do.
	KeysAlreadyInProgress int
	// KeysAlreadyAvailable is the number of keys whose storage class
	// doesn't need restoration (STANDARD, GLACIER_IR with active
	// restore, etc. — i.e. ErrNotArchived).
	KeysAlreadyAvailable int
	// FilesAffected is the number of DB rows covered by the requested
	// keys (zip archives count their members).
	FilesAffected int64
	// BytesAffected is the cumulative size of the affected DB rows.
	BytesAffected int64
	// FilesSkippedInProgress / FilesSkippedRestored are matched rows we
	// chose NOT to issue a fresh RestoreObject for because their local
	// restore_status indicates AWS already has (or is producing) a
	// thawed copy. Re-requesting would just extend the AWS expiry and
	// re-bill retrieval, so we skip + surface a count to the operator.
	FilesSkippedInProgress int64
	BytesSkippedInProgress int64
	FilesSkippedRestored   int64
	BytesSkippedRestored   int64
	UnknownPaths           []string
	Errors                 []string
}

// RequestRestore issues a Glacier restore (s3:RestoreObject) for every
// unique S3 key covering the matched DB rows, asking AWS to keep the
// thawed copy in the standard tier for opts.Days days. Multiple files
// inside one zip share a single restore call. Successful new requests
// flip the matching DB rows to restore_status='in_progress' so the UI
// reflects state immediately rather than waiting for the SQS Restore
// Initiated event.
//
// Per-key errors are collected in RestoreRequestStats.Errors; the
// function only returns a hard error for setup failures.
func RequestRestore(ctx context.Context, opts RestoreRequestOptions) (RestoreRequestStats, error) {
	var stats RestoreRequestStats
	if opts.DB == nil {
		return stats, errors.New("restore: DB is required")
	}
	if opts.Storage == nil {
		return stats, errors.New("restore: Storage is required")
	}
	if opts.Days < 1 {
		return stats, fmt.Errorf("restore: Days must be >= 1 (got %d)", opts.Days)
	}
	switch opts.Tier {
	case "", storage.RestoreTierStandard:
		opts.Tier = storage.RestoreTierStandard
	case storage.RestoreTierBulk:
	default:
		return stats, fmt.Errorf("restore: unknown restore tier %q", opts.Tier)
	}

	wantAll := false
	wantSet := make(map[string]struct{}, len(opts.Paths))
	for _, p := range opts.Paths {
		if p == "" || p == "/" {
			wantAll = true
			break
		}
		wantSet[p] = struct{}{}
	}

	// Group matching rows by the S3 key that would actually be restored —
	// the zip archive's key for zipped members, or the row's own s3_key
	// for individually-uploaded files.
	type keyGroup struct {
		fileCount int64
		bytes     int64
		s3Keys    []string // for MarkRestoreInProgress on success
	}
	byKey := map[string]*keyGroup{}
	matched := map[string]bool{}

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
			if f.Status != db.StatusUploaded && f.Status != db.StatusZipped {
				continue
			}
			if !wantAll {
				hit := false
				for req := range wantSet {
					if pathutil.HasPrefixPath(f.Path, req) {
						hit = true
						matched[req] = true
						break
					}
				}
				if !hit {
					continue
				}
			}
			// Skip rows AWS already has thawed (or is thawing) — issuing a
			// fresh RestoreObject would just extend the standard-tier
			// expiry and re-bill retrieval. Track the skipped totals so
			// the UI can show "X files skipped because already thawed".
			switch f.RestoreStatus {
			case db.RestoreStatusInProgress:
				stats.FilesSkippedInProgress++
				stats.BytesSkippedInProgress += f.Size
				continue
			case db.RestoreStatusRestored:
				stats.FilesSkippedRestored++
				stats.BytesSkippedRestored += f.Size
				continue
			}
			var key string
			if f.ZipName != "" {
				key = pathutil.JoinKey(opts.KeyPrefix, f.ZipName)
			} else if f.S3Key != "" {
				key = f.S3Key
			} else {
				continue
			}
			g, ok := byKey[key]
			if !ok {
				g = &keyGroup{}
				byKey[key] = g
			}
			g.fileCount++
			g.bytes += f.Size
			if f.S3Key != "" {
				g.s3Keys = append(g.s3Keys, f.S3Key)
			}
		}
		if len(rows) < pageSize {
			break
		}
	}
	if !wantAll {
		for req := range wantSet {
			if !matched[req] {
				stats.UnknownPaths = append(stats.UnknownPaths, req)
			}
		}
	}

	// Sort keys so the output is deterministic + so a partial cancellation
	// processes them in a predictable order.
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	emit := opts.Emit
	if emit == nil {
		emit = DiscardEvents
	}
	total := len(keys)
	emit(Event{
		Type: EventRestoreRequestStart,
		At:   time.Now(),
		Data: map[string]any{"total": total},
	})

	for i, key := range keys {
		if err := ctx.Err(); err != nil {
			emit(Event{
				Type: EventRestoreRequestFailed,
				At:   time.Now(),
				Data: map[string]any{"error": err.Error(), "processed": i, "total": total},
			})
			return stats, err
		}
		g := byKey[key]
		stats.FilesAffected += g.fileCount
		stats.BytesAffected += g.bytes

		err := opts.Storage.Restore(ctx, key, opts.Days, opts.Tier)
		switch {
		case err == nil:
			stats.KeysRequested++
			// Flip every covered s3_key to in_progress so the UI reflects
			// the new state without waiting on the SQS event. One bulk
			// UPDATE per group — the previous per-file loop was O(N) commits
			// and made a 2000-file zip restore take minutes. (#restore-batch)
			if _, mErr := opts.DB.MarkRestoreInProgressMany(ctx, g.s3Keys); mErr != nil {
				stats.Errors = append(stats.Errors, key+": mark in_progress: "+mErr.Error())
			}
		case errors.Is(err, storage.ErrRestoreInProgress):
			stats.KeysAlreadyInProgress++
			_, _ = opts.DB.MarkRestoreInProgressMany(ctx, g.s3Keys)
		case errors.Is(err, storage.ErrNotArchived):
			stats.KeysAlreadyAvailable++
		default:
			stats.Errors = append(stats.Errors, key+": "+err.Error())
		}

		processed := i + 1
		if processed == total || processed%restoreRequestEmitEvery == 0 {
			emit(Event{
				Type: EventRestoreRequestProgress,
				At:   time.Now(),
				Data: map[string]any{
					"processed":           processed,
					"total":               total,
					"keys_requested":      stats.KeysRequested,
					"keys_already_thawed": stats.KeysAlreadyInProgress + stats.KeysAlreadyAvailable,
					"errors":              len(stats.Errors),
				},
			})
		}
	}

	emit(Event{
		Type: EventRestoreRequestComplete,
		At:   time.Now(),
		Data: map[string]any{
			"total":                    total,
			"keys_requested":           stats.KeysRequested,
			"keys_already_in_progress": stats.KeysAlreadyInProgress,
			"keys_already_available":   stats.KeysAlreadyAvailable,
			"files_affected":           stats.FilesAffected,
			"bytes_affected":           stats.BytesAffected,
			"errors":                   len(stats.Errors),
		},
	})
	return stats, nil
}

// RestoreToDir downloads every restored DB file matching opts.Paths and
// writes it beneath opts.TargetDir. Files stored individually are
// streamed from their s3_key; files inside a zip archive are downloaded
// once per zip and extracted selectively. Rows that are still thawing or
// not marked restored are skipped rather than triggering a failing GET.
//
// Non-fatal per-file errors are collected in RestoreStats.Errors; the
// function only returns a hard error for setup failures (bad target,
// unreadable DB).
func RestoreToDir(ctx context.Context, opts RestoreOptions) (stats RestoreStats, err error) {
	if opts.DB == nil {
		return stats, errors.New("restore: DB is required")
	}
	if opts.Storage == nil {
		return stats, errors.New("restore: Storage is required")
	}
	if opts.TargetDir == "" || !filepath.IsAbs(opts.TargetDir) {
		return stats, fmt.Errorf("restore: TargetDir must be an absolute path, got %q", opts.TargetDir)
	}
	if err := os.MkdirAll(opts.TargetDir, 0o755); err != nil {
		return stats, fmt.Errorf("mkdir target: %w", err)
	}
	cleanTarget, err := filepath.Abs(filepath.Clean(opts.TargetDir))
	if err != nil {
		return stats, fmt.Errorf("abs target: %w", err)
	}
	emit := opts.Emit
	if emit == nil {
		emit = DiscardEvents
	}
	started := false
	defer func() {
		if !started {
			return
		}
		evType := EventRestoreDownloadComplete
		data := map[string]any{
			"files_written": stats.FilesWritten,
			"bytes_written": stats.BytesWritten,
			"errors":        len(stats.Errors),
		}
		if err != nil {
			evType = EventRestoreDownloadFailed
			data["error"] = err.Error()
		}
		emit(Event{
			Type: evType,
			At:   time.Now(),
			Data: data,
		})
	}()

	// Collect matching rows from the DB, grouped by zip membership.
	wantAll := false
	wantSet := make(map[string]struct{}, len(opts.Paths))
	for _, p := range opts.Paths {
		if p == "" || p == "/" {
			wantAll = true
			break
		}
		wantSet[p] = struct{}{}
	}

	byZip := map[string][]db.File{} // zip_name -> rows; "" key = standalone
	matched := map[string]bool{}    // which want-paths saw a hit
	total := 0

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
			if !wantAll {
				hit := false
				for req := range wantSet {
					if pathutil.HasPrefixPath(f.Path, req) {
						hit = true
						matched[req] = true
						break
					}
				}
				if !hit {
					continue
				}
			}
			if f.ZipName == "" && f.S3Key == "" {
				// Never uploaded — nothing to restore.
				stats.Skipped = append(stats.Skipped, f.Path+" (never uploaded)")
				continue
			}
			switch f.RestoreStatus {
			case db.RestoreStatusRestored:
				// downloadable now
			case db.RestoreStatusInProgress:
				stats.Skipped = append(stats.Skipped, f.Path+" (still thawing)")
				continue
			default:
				stats.Skipped = append(stats.Skipped, f.Path+" (not restored yet)")
				continue
			}
			byZip[f.ZipName] = append(byZip[f.ZipName], f)
			total++
		}
		if len(rows) < pageSize {
			break
		}
	}
	if !wantAll {
		for req := range wantSet {
			if !matched[req] {
				stats.Skipped = append(stats.Skipped, req+" (no matching files)")
			}
		}
	}

	emit(Event{
		Type: EventRestoreDownloadStart,
		At:   time.Now(),
		Data: map[string]any{
			"total": total,
		},
	})
	started = true

	// Standalone files first — one GET per file, cheaper than zips.
	processed := 0
	if rows := byZip[""]; len(rows) > 0 {
		sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
		for _, f := range rows {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			expected := f.MD5
			if opts.SkipChecksum {
				expected = ""
			}
			n, err := restoreStandalone(ctx, opts.Storage, cleanTarget, f, expected)
			processed++
			if err != nil {
				stats.Errors = append(stats.Errors, f.Path+": "+err.Error())
				emit(Event{
					Type: EventRestoreDownloadProgress,
					At:   time.Now(),
					Data: map[string]any{
						"path":          f.Path,
						"processed":     processed,
						"total":         total,
						"files_written": stats.FilesWritten,
						"bytes_written": stats.BytesWritten,
						"errors":        len(stats.Errors),
						"error":         err.Error(),
					},
				})
				continue
			}
			stats.FilesWritten++
			stats.BytesWritten += n
			emit(Event{
				Type: EventRestoreDownloadProgress,
				At:   time.Now(),
				Data: map[string]any{
					"path":          f.Path,
					"processed":     processed,
					"total":         total,
					"files_written": stats.FilesWritten,
					"bytes_written": stats.BytesWritten,
					"errors":        len(stats.Errors),
				},
			})
		}
	}

	// Zipped files — download each zip once, extract all wanted entries.
	zipNames := make([]string, 0, len(byZip))
	for z := range byZip {
		if z == "" {
			continue
		}
		zipNames = append(zipNames, z)
	}
	sort.Strings(zipNames)
	for _, z := range zipNames {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		written, bytes, errs, nextProcessed := restoreZipMembers(ctx, opts, cleanTarget, z, byZip[z], processed, total, emit)
		processed = nextProcessed
		stats.FilesWritten += written
		stats.BytesWritten += bytes
		stats.Errors = append(stats.Errors, errs...)
	}

	return stats, nil
}

// restoreStandalone downloads a single individually-uploaded file into
// the target directory. Returns bytes written.
func restoreStandalone(ctx context.Context, s storage.Storage, target string, f db.File, expectedMD5 string) (int64, error) {
	dst, err := safeJoin(target, f.Path)
	if err != nil {
		return 0, err
	}
	rc, err := s.Get(ctx, f.S3Key)
	if err != nil {
		if errors.Is(err, storage.ErrGlacierThawing) {
			return 0, fmt.Errorf("get %s: still thawing from glacier: %w", f.S3Key, err)
		}
		return 0, fmt.Errorf("get %s: %w", f.S3Key, err)
	}
	defer rc.Close()
	return writeFromReader(dst, rc, expectedMD5)
}

// restoreZipMembers downloads the zip at zipName once and extracts every
// matching member listed in members. Errors are returned per-member so a
// corrupt entry doesn't poison the rest of the archive.
func restoreZipMembers(ctx context.Context, opts RestoreOptions, target, zipName string, members []db.File, processed, total int, emit EventEmitter) (written, bytes int64, errs []string, nextProcessed int) {
	key := pathutil.JoinKey(opts.KeyPrefix, zipName)

	tmp, err := os.CreateTemp(opts.TmpDir, "aws-backup-restore-*.zip")
	if err != nil {
		errs = append(errs, zipName+": create temp: "+err.Error())
		return written, bytes, errs, processed
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	rc, err := opts.Storage.Get(ctx, key)
	if err != nil {
		tmp.Close()
		if errors.Is(err, storage.ErrGlacierThawing) {
			errs = append(errs, zipName+": get zip "+key+": still thawing from glacier: "+err.Error())
		} else {
			errs = append(errs, zipName+": get zip "+key+": "+err.Error())
		}
		return written, bytes, errs, processed
	}
	if _, err := io.Copy(tmp, rc); err != nil {
		rc.Close()
		tmp.Close()
		errs = append(errs, zipName+": download zip "+key+": "+err.Error())
		return written, bytes, errs, processed
	}
	rc.Close()
	if err := tmp.Close(); err != nil {
		errs = append(errs, zipName+": close temp zip: "+err.Error())
		return written, bytes, errs, processed
	}

	zr, err := zip.OpenReader(tmpPath)
	if err != nil {
		errs = append(errs, zipName+": open zip: "+err.Error())
		return written, bytes, errs, processed
	}
	defer zr.Close()

	byName := map[string]*zip.File{}
	// Build a case-folded index alongside the exact map so that DBs
	// rebuilt from S3 listings on case-insensitive filesystems can still
	// resolve members when the on-disk path differs only in case. We
	// also detect ambiguous case-insensitive collisions so we don't
	// pick the wrong file silently. (#193)
	byFolded := map[string]*zip.File{}
	foldCollision := map[string]bool{}
	for _, zf := range zr.File {
		byName[zf.Name] = zf
		k := strings.ToLower(zf.Name)
		if _, dup := byFolded[k]; dup {
			foldCollision[k] = true
		} else {
			byFolded[k] = zf
		}
	}

	for _, m := range members {
		if err := ctx.Err(); err != nil {
			errs = append(errs, m.Path+": "+err.Error())
			return written, bytes, errs, processed
		}
		zf, ok := byName[m.Path]
		if !ok {
			fk := strings.ToLower(m.Path)
			if foldCollision[fk] {
				errs = append(errs, m.Path+": ambiguous case-insensitive match in zip "+zipName)
				processed++
				emit(Event{
					Type: EventRestoreDownloadProgress,
					At:   time.Now(),
					Data: map[string]any{
						"path":          m.Path,
						"processed":     processed,
						"total":         total,
						"files_written": written,
						"bytes_written": bytes,
						"errors":        len(errs),
						"error":         "ambiguous case-insensitive match in zip " + zipName,
					},
				})
				continue
			}
			if alt, found := byFolded[fk]; found {
				slog.Info("restore: case-insensitive zip member match",
					"requested", m.Path, "used", alt.Name, "zip", zipName)
				zf = alt
			} else {
				errs = append(errs, m.Path+": not found in zip "+zipName)
				processed++
				emit(Event{
					Type: EventRestoreDownloadProgress,
					At:   time.Now(),
					Data: map[string]any{
						"path":          m.Path,
						"processed":     processed,
						"total":         total,
						"files_written": written,
						"bytes_written": bytes,
						"errors":        len(errs),
						"error":         "not found in zip " + zipName,
					},
				})
				continue
			}
		}
		dst, err := safeJoin(target, m.Path)
		if err != nil {
			errs = append(errs, m.Path+": "+err.Error())
			processed++
			emit(Event{
				Type: EventRestoreDownloadProgress,
				At:   time.Now(),
				Data: map[string]any{
					"path":          m.Path,
					"processed":     processed,
					"total":         total,
					"files_written": written,
					"bytes_written": bytes,
					"errors":        len(errs),
					"error":         err.Error(),
				},
			})
			continue
		}
		entry, err := zf.Open()
		if err != nil {
			errs = append(errs, m.Path+": open entry: "+err.Error())
			processed++
			emit(Event{
				Type: EventRestoreDownloadProgress,
				At:   time.Now(),
				Data: map[string]any{
					"path":          m.Path,
					"processed":     processed,
					"total":         total,
					"files_written": written,
					"bytes_written": bytes,
					"errors":        len(errs),
					"error":         err.Error(),
				},
			})
			continue
		}
		expected := m.MD5
		if opts.SkipChecksum {
			expected = ""
		}
		n, err := writeFromReader(dst, entry, expected)
		entry.Close()
		if err != nil {
			errs = append(errs, m.Path+": write: "+err.Error())
			processed++
			emit(Event{
				Type: EventRestoreDownloadProgress,
				At:   time.Now(),
				Data: map[string]any{
					"path":          m.Path,
					"processed":     processed,
					"total":         total,
					"files_written": written,
					"bytes_written": bytes,
					"errors":        len(errs),
					"error":         err.Error(),
				},
			})
			continue
		}
		// Preserve the zip entry's file mode (executable bit, restrictive
		// perms, etc.) — os.Create defaults to 0666 & umask, which silently
		// drops the executable bit on every restored binary. (#228)
		if mode := zf.Mode().Perm(); mode != 0 {
			if cerr := os.Chmod(dst, mode); cerr != nil {
				errs = append(errs, m.Path+": chmod: "+cerr.Error())
			}
		}
		written++
		bytes += n
		processed++
		emit(Event{
			Type: EventRestoreDownloadProgress,
			At:   time.Now(),
			Data: map[string]any{
				"path":          m.Path,
				"processed":     processed,
				"total":         total,
				"files_written": written,
				"bytes_written": bytes,
				"errors":        len(errs),
			},
		})
	}
	return written, bytes, errs, processed
}

// writeFromReader copies src to a new file at dst (creating parent
// directories). When expectedMD5 is non-empty, the bytes are hashed
// in-flight and compared against it; a mismatch returns an error so
// the caller can surface it in RestoreStats.Errors. The file is left
// in place on mismatch — the operator decides whether to keep or
// delete it. (#75)
//
// On any *other* error (ctx cancel, io error, close error) the partial
// file is removed so a zero-byte / truncated dst doesn't masquerade as
// a successfully restored tiny file on retry. (#194)
//
// Returns the number of bytes written.
func writeFromReader(dst string, src io.Reader, expectedMD5 string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}
	out, err := os.Create(dst)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", dst, err)
	}
	var (
		w io.Writer = out
		h hash.Hash
	)
	if expectedMD5 != "" {
		h = md5.New()
		w = io.MultiWriter(out, h)
	}
	n, err := io.Copy(w, src)
	if cerr := out.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return n, err
	}
	if h != nil {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, expectedMD5) {
			return n, fmt.Errorf("checksum mismatch: got md5 %s, want %s", got, expectedMD5)
		}
	}
	return n, nil
}

// safeJoin resolves relPath beneath root and guards against path
// traversal. Returns an error when the result would escape root — e.g. a
// DB row with "../../etc/passwd" must never write outside target.
func safeJoin(root, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("empty relative path")
	}
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") ||
		clean == ".." || strings.Contains(clean, string(filepath.Separator)+".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path %q", relPath)
	}
	joined := filepath.Join(root, clean)
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path %q escapes target", relPath)
	}
	// filepath.Abs is purely lexical; resolve any symlinks present in
	// existing ancestors and re-check the result is still under root so
	// a symlink like `<root>/photos -> /etc` can't redirect a write
	// outside target. ENOENT on the leaf or deeper components is fine
	// (we're about to create them). (#218)
	rootResolved, rerr := filepath.EvalSymlinks(root)
	if rerr != nil {
		return "", fmt.Errorf("resolve target root: %w", rerr)
	}
	probe := abs
	for {
		if probe == rootResolved || probe == filepath.Dir(probe) {
			break
		}
		resolved, rerr := filepath.EvalSymlinks(probe)
		if rerr == nil {
			rel2, _ := filepath.Rel(rootResolved, resolved)
			if rel2 == ".." || strings.HasPrefix(rel2, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("unsafe path %q: ancestor symlink escapes target", relPath)
			}
			break
		}
		if !os.IsNotExist(rerr) {
			return "", fmt.Errorf("resolve %q: %w", probe, rerr)
		}
		probe = filepath.Dir(probe)
	}
	return abs, nil
}
