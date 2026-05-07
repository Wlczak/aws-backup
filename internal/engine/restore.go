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
}

// RestoreStats summarizes the outcome of RestoreToDir. Errors are
// returned per-file so one bad object doesn't abort a bulk restore.
type RestoreStats struct {
	FilesWritten int64
	BytesWritten int64
	Skipped      []string
	Errors       []string
}

// RestoreToDir downloads every DB file matching opts.Paths and writes it
// beneath opts.TargetDir. Files stored individually are streamed from
// their s3_key; files inside a zip archive are downloaded once per zip
// and extracted selectively.
//
// Non-fatal per-file errors are collected in RestoreStats.Errors; the
// function only returns a hard error for setup failures (bad target,
// unreadable DB).
func RestoreToDir(ctx context.Context, opts RestoreOptions) (RestoreStats, error) {
	var stats RestoreStats
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
			byZip[f.ZipName] = append(byZip[f.ZipName], f)
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

	// Standalone files first — one GET per file, cheaper than zips.
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
			if err != nil {
				stats.Errors = append(stats.Errors, f.Path+": "+err.Error())
				continue
			}
			stats.FilesWritten++
			stats.BytesWritten += n
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
		written, bytes, errs := restoreZipMembers(ctx, opts, cleanTarget, z, byZip[z])
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
func restoreZipMembers(ctx context.Context, opts RestoreOptions, target, zipName string, members []db.File) (written, bytes int64, errs []string) {
	key := pathutil.JoinKey(opts.KeyPrefix, zipName)

	tmp, err := os.CreateTemp(opts.TmpDir, "aws-backup-restore-*.zip")
	if err != nil {
		errs = append(errs, zipName+": create temp: "+err.Error())
		return
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
		return
	}
	if _, err := io.Copy(tmp, rc); err != nil {
		rc.Close()
		tmp.Close()
		errs = append(errs, zipName+": download zip "+key+": "+err.Error())
		return
	}
	rc.Close()
	if err := tmp.Close(); err != nil {
		errs = append(errs, zipName+": close temp zip: "+err.Error())
		return
	}

	zr, err := zip.OpenReader(tmpPath)
	if err != nil {
		errs = append(errs, zipName+": open zip: "+err.Error())
		return
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
			return
		}
		zf, ok := byName[m.Path]
		if !ok {
			fk := strings.ToLower(m.Path)
			if foldCollision[fk] {
				errs = append(errs, m.Path+": ambiguous case-insensitive match in zip "+zipName)
				continue
			}
			if alt, found := byFolded[fk]; found {
				slog.Info("restore: case-insensitive zip member match",
					"requested", m.Path, "used", alt.Name, "zip", zipName)
				zf = alt
			} else {
				errs = append(errs, m.Path+": not found in zip "+zipName)
				continue
			}
		}
		dst, err := safeJoin(target, m.Path)
		if err != nil {
			errs = append(errs, m.Path+": "+err.Error())
			continue
		}
		entry, err := zf.Open()
		if err != nil {
			errs = append(errs, m.Path+": open entry: "+err.Error())
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
	}
	return
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
	return abs, nil
}
