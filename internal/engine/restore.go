package engine

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
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
			n, err := restoreStandalone(ctx, opts.Storage, cleanTarget, f)
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
func restoreStandalone(ctx context.Context, s storage.Storage, target string, f db.File) (int64, error) {
	dst, err := safeJoin(target, f.Path)
	if err != nil {
		return 0, err
	}
	rc, err := s.Get(ctx, f.S3Key)
	if err != nil {
		return 0, fmt.Errorf("get %s: %w", f.S3Key, err)
	}
	defer rc.Close()
	return writeFromReader(dst, rc)
}

// restoreZipMembers downloads the zip at zipName once and extracts every
// matching member listed in members. Errors are returned per-member so a
// corrupt entry doesn't poison the rest of the archive.
func restoreZipMembers(ctx context.Context, opts RestoreOptions, target, zipName string, members []db.File) (written, bytes int64, errs []string) {
	key := pathutil.JoinKey(opts.KeyPrefix, zipName)

	tmp, err := os.CreateTemp("", "aws-backup-restore-*.zip")
	if err != nil {
		errs = append(errs, zipName+": create temp: "+err.Error())
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	rc, err := opts.Storage.Get(ctx, key)
	if err != nil {
		tmp.Close()
		errs = append(errs, zipName+": get zip "+key+": "+err.Error())
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
	for _, zf := range zr.File {
		byName[zf.Name] = zf
	}

	for _, m := range members {
		if err := ctx.Err(); err != nil {
			errs = append(errs, m.Path+": "+err.Error())
			return
		}
		zf, ok := byName[m.Path]
		if !ok {
			errs = append(errs, m.Path+": not found in zip "+zipName)
			continue
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
		n, err := writeFromReader(dst, entry)
		entry.Close()
		if err != nil {
			errs = append(errs, m.Path+": write: "+err.Error())
			continue
		}
		written++
		bytes += n
	}
	return
}

// writeFromReader copies src to a new file at dst (creating parent
// directories). Returns the number of bytes written.
func writeFromReader(dst string, src io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}
	out, err := os.Create(dst)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", dst, err)
	}
	n, err := io.Copy(out, src)
	if cerr := out.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return n, err
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

