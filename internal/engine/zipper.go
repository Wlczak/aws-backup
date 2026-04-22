// Package engine hosts the backup orchestrator and its pieces: grouping
// pending files by top-level directory, zipping those groups, and
// eventually (see engine.go) driving the scan -> zip -> upload pipeline.
package engine

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Wlczak/aws-backup/internal/source"
)

// PendingFile is what the engine passes to the zipper / uploader: a row
// in the `files` table that is ready to be packaged.
type PendingFile struct {
	ID      int64
	RelPath string // source-relative path, forward slashes
	Size    int64
}

// Group is the output of GroupFiles — a set of files sharing a top-level
// directory, plus a decision on whether to zip them or upload each file
// on its own.
type Group struct {
	TopDir string // top-level directory, "" for root-level files
	Zip    bool
	Files  []PendingFile
}

// rootDirLabel is the fake "directory" name used when naming zips that
// contain root-level files.
const rootDirLabel = "_root"

// GroupFiles splits pending files by their top-level directory and decides
// per-group whether to zip. A group is zipped when its file count is
// >= zipThreshold. Groups below the threshold upload each file individually.
//
// Groups are returned in alphabetical TopDir order so output is deterministic.
func GroupFiles(files []PendingFile, zipThreshold int) []Group {
	if zipThreshold <= 0 {
		zipThreshold = 1 // treat 0 as "always zip"; 1 means any non-empty group
	}
	byDir := map[string][]PendingFile{}
	for _, f := range files {
		top := topDir(f.RelPath)
		byDir[top] = append(byDir[top], f)
	}

	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	out := make([]Group, 0, len(dirs))
	for _, d := range dirs {
		group := byDir[d]
		sort.Slice(group, func(i, j int) bool { return group[i].RelPath < group[j].RelPath })
		out = append(out, Group{
			TopDir: d,
			Zip:    len(group) >= zipThreshold,
			Files:  group,
		})
	}
	return out
}

// topDir returns the first forward-slash-separated component of p, or ""
// for root-level files.
func topDir(p string) string {
	p = strings.TrimPrefix(p, "/")
	i := strings.IndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return p[:i]
}

// ZipName derives an archive filename from the files' longest common
// directory path and a per-run sequential index:
//
//	backup_folder1_images_1.zip
//
// Slashes in the common directory are replaced with underscores so the
// name reflects the full folder hierarchy. If files have no common
// directory (or are at the root), the "_root" prefix is used.
func ZipName(files []PendingFile, n int) string {
	d := commonDirLabel(files)
	return fmt.Sprintf("%s_%d.zip", d, n)
}

// commonDirLabel finds the longest directory path shared by all files,
// sanitizes it (slashes → underscores), and returns it. Returns "_root"
// when files share no common directory.
func commonDirLabel(files []PendingFile) string {
	if len(files) == 0 {
		return rootDirLabel
	}
	dirParts := func(p string) []string {
		segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
		if len(segs) <= 1 {
			return nil // root-level file, no directory
		}
		return segs[:len(segs)-1]
	}
	common := dirParts(files[0].RelPath)
	for _, f := range files[1:] {
		d := dirParts(f.RelPath)
		n := len(common)
		if len(d) < n {
			n = len(d)
		}
		keep := 0
		for i := 0; i < n; i++ {
			if common[i] == d[i] {
				keep = i + 1
			} else {
				break
			}
		}
		common = common[:keep]
	}
	if len(common) == 0 {
		return rootDirLabel
	}
	return sanitizeLabel(strings.Join(common, "/"))
}

// sanitizeLabel keeps [A-Za-z0-9._-], replaces everything else with "_".
func sanitizeLabel(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
		if ok {
			b = append(b, c)
		} else {
			b = append(b, '_')
		}
	}
	if len(b) == 0 {
		return rootDirLabel
	}
	return string(b)
}

// CreateZip streams every file in `files` from src into a zip at outPath.
// Files retain their source-relative RelPath inside the archive. Returns
// the final zip size and the list of entry paths written.
func CreateZip(ctx context.Context, src source.Source, files []PendingFile, outPath string) (size int64, entries []string, err error) {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return 0, nil, err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
	}()

	zw := zip.NewWriter(out)
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			zw.Close()
			return 0, nil, err
		}
		if err := writeZipEntry(ctx, zw, src, f); err != nil {
			zw.Close()
			return 0, nil, fmt.Errorf("zip %s: %w", f.RelPath, err)
		}
		entries = append(entries, f.RelPath)
	}
	if err := zw.Close(); err != nil {
		return 0, nil, err
	}
	if err := out.Sync(); err != nil {
		return 0, nil, err
	}
	st, err := out.Stat()
	if err != nil {
		return 0, nil, err
	}
	return st.Size(), entries, nil
}

func writeZipEntry(ctx context.Context, zw *zip.Writer, src source.Source, f PendingFile) error {
	w, err := zw.Create(f.RelPath)
	if err != nil {
		return err
	}
	rc, err := src.Open(ctx, f.RelPath)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(w, rc)
	return err
}
