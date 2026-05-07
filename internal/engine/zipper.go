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
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wlczak/aws-backup/internal/source"
)

// PendingFile is what the engine passes to the zipper / uploader: a row
// in the `files` table that is ready to be packaged.
type PendingFile struct {
	ID      int64
	RelPath string // source-relative path, forward slashes
	Size    int64
	// MTime is the source file's mtime as recorded by the most recent
	// scan. Used by uploadIndividual's resume path to decide whether a
	// cached tmp file is still fresh: if tmp.ModTime() < MTime, the
	// source has changed since the cached copy was written. (#127)
	MTime time.Time
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

// dirNode is one level of the path tree built by GroupFiles. Each node
// tracks the files directly in it (not in subdirectories) plus a sorted
// map of child subdirectories. size/count are the cumulative totals for
// the subtree, used to decide whether the subtree can become a single
// zip or has to be split.
type dirNode struct {
	files    []PendingFile
	children map[string]*dirNode
	size     int64
	count    int
}

// GroupFiles splits pending files into upload groups, preferring
// subdirectory boundaries over arbitrary numbered slices.
//
// Algorithm:
//  1. Build a path tree from all RelPaths.
//  2. Walk the tree: if a subtree's cumulative size fits in maxBytes,
//     emit it as one group (zipped if count >= zipThreshold, otherwise
//     individual uploads).
//  3. If a subtree is too big, recurse into each child subdirectory —
//     so `photos/2024/` and `photos/2025/` become separate zips instead
//     of one monolithic `photos_1.zip`. Child subdirectories with fewer
//     than minZipDirFiles files are folded into the parent's loose-file
//     pool instead of becoming tiny standalone groups.
//  4. Files sitting directly at a splitting level ("loose files"), plus
//     any folded small-child files, form their own group; if they
//     collectively exceed maxBytes they are chunked by size.
//
// maxBytes <= 0 disables the size cap. minZipDirFiles <= 0 disables the
// small-child folding. Groups are returned in deterministic path order.
func GroupFiles(files []PendingFile, zipThreshold, minZipDirFiles int, maxBytes int64) []Group {
	if zipThreshold <= 0 {
		zipThreshold = 1
	}
	if len(files) == 0 {
		return nil
	}

	root := buildTree(files)
	var out []Group
	// Root always descends into its children so each top-level directory
	// is a natural group boundary, independent of the size cap. The cap
	// only drives further splits inside a top-level subtree.
	walkTree(root, "", zipThreshold, minZipDirFiles, maxBytes, true, &out)
	return out
}

// buildTree organises files into a dirNode tree keyed by path components.
// Files at "" RelPath level sit directly under root.
func buildTree(files []PendingFile) *dirNode {
	root := &dirNode{children: map[string]*dirNode{}}
	for _, f := range files {
		parts := splitPath(f.RelPath)
		node := root
		// Walk down to the parent of the file, creating nodes as needed.
		for i := 0; i < len(parts)-1; i++ {
			name := parts[i]
			child, ok := node.children[name]
			if !ok {
				child = &dirNode{children: map[string]*dirNode{}}
				node.children[name] = child
			}
			node = child
		}
		node.files = append(node.files, f)
		// Propagate size/count to every ancestor.
		n := root
		n.size += f.Size
		n.count++
		for i := 0; i < len(parts)-1; i++ {
			n = n.children[parts[i]]
			n.size += f.Size
			n.count++
		}
	}
	return root
}

// walkTree decides how to split a subtree into groups, writing them into
// *out. pathPrefix is the slash-joined directory path from the tree root
// to this node ("" for root). mustDescend forces the split at this level
// even if the subtree would fit in the cap — set at root so top-level
// directories are always their own groups regardless of cap settings.
func walkTree(n *dirNode, pathPrefix string, zipThreshold, minZipDirFiles int, maxBytes int64, mustDescend bool, out *[]Group) {
	// Subtree fits in the cap — emit the whole thing as one group.
	if !mustDescend && (maxBytes <= 0 || n.size <= maxBytes) {
		files := collectSubtree(n)
		if len(files) == 0 {
			return
		}
		sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
		*out = append(*out, Group{
			TopDir: pathPrefix,
			Zip:    len(files) >= zipThreshold,
			Files:  files,
		})
		return
	}

	// Descend into each child subdir so subfolders become the group
	// boundary (e.g. photos/2024/ vs photos/2025/) instead of numbered
	// slices of a flat top-dir. Children with fewer than minZipDirFiles
	// files (only when not at the forced root level) are folded into
	// this level's loose-file pool rather than becoming tiny groups.
	childNames := make([]string, 0, len(n.children))
	for name := range n.children {
		childNames = append(childNames, name)
	}
	sort.Strings(childNames)

	var folded []PendingFile
	for _, name := range childNames {
		child := n.children[name]
		childPrefix := name
		if pathPrefix != "" {
			childPrefix = pathPrefix + "/" + name
		}
		if !mustDescend && minZipDirFiles > 0 && child.count < minZipDirFiles {
			folded = append(folded, collectSubtree(child)...)
			continue
		}
		walkTree(child, childPrefix, zipThreshold, minZipDirFiles, maxBytes, false, out)
	}

	// Loose files at this level plus any folded small-child files form
	// their own group. If they exceed the cap, chunk them by size.
	loose := append(append([]PendingFile(nil), n.files...), folded...)
	if len(loose) == 0 {
		return
	}
	sort.Slice(loose, func(i, j int) bool { return loose[i].RelPath < loose[j].RelPath })

	var looseSize int64
	for _, f := range loose {
		looseSize += f.Size
	}
	if maxBytes <= 0 || looseSize <= maxBytes {
		*out = append(*out, Group{
			TopDir: pathPrefix,
			Zip:    len(loose) >= zipThreshold,
			Files:  loose,
		})
		return
	}

	// Chunker: accumulate files until adding the next would exceed cap,
	// then flush. A single file larger than maxBytes becomes its own
	// individual-upload group (zipping it alone saves nothing).
	var cur []PendingFile
	var curSize int64
	flush := func(zip bool) {
		if len(cur) == 0 {
			return
		}
		*out = append(*out, Group{TopDir: pathPrefix, Zip: zip, Files: cur})
		cur = nil
		curSize = 0
	}
	for _, f := range loose {
		if f.Size > maxBytes {
			flush(true)
			*out = append(*out, Group{TopDir: pathPrefix, Zip: false, Files: []PendingFile{f}})
			continue
		}
		if curSize+f.Size > maxBytes && len(cur) > 0 {
			flush(true)
		}
		cur = append(cur, f)
		curSize += f.Size
	}
	flush(true)
}

// collectSubtree returns every file under n in no particular order; the
// caller sorts.
func collectSubtree(n *dirNode) []PendingFile {
	out := make([]PendingFile, 0, n.count)
	var walk func(*dirNode)
	walk = func(m *dirNode) {
		out = append(out, m.files...)
		for _, c := range m.children {
			walk(c)
		}
	}
	walk(n)
	return out
}

// splitPath turns "a/b/c.txt" into ["a","b","c.txt"]; a leading slash is
// ignored so absolute-looking relpaths group the same way.
func splitPath(p string) []string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
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

// ZipRelPath returns the zip's path relative to the configured S3 key
// prefix. The directory portion mirrors the files' longest common source
// directory (slashes preserved, one sanitized segment per component); the
// filename portion is ZipName's existing "<label>_N.zip". Callers combine
// this with the key prefix via path.Join to get the final S3 key, and also
// store it as `files.zip_name` in the DB so lookups keep working.
//
// Example: files under "photos/2024/" with prefix "backups/" produce
//
//	zip_name = "photos/2024/photos_2024_1.zip"
//	S3 key   = "backups/photos/2024/photos_2024_1.zip"
func ZipRelPath(files []PendingFile, n int) string {
	name := ZipName(files, n)
	dir := commonDirPath(files)
	if dir == "" {
		return name
	}
	return path.Join(dir, name)
}

// parseZipNumber extracts the trailing integer N from a zip relative path
// whose filename follows the "<label>_N.zip" convention (e.g.
// "photos/2024/photos_2024_3.zip" → 3). Returns 0 on any parse failure.
func parseZipNumber(zipRelPath string) int {
	base := strings.TrimSuffix(path.Base(zipRelPath), ".zip")
	i := strings.LastIndex(base, "_")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(base[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// commonDirPath finds the longest directory shared by all files and
// returns it as a slash-joined path with each segment sanitized
// individually (slashes preserved). Returns "" when files share no
// common directory.
func commonDirPath(files []PendingFile) string {
	segs := commonDirSegments(files)
	if len(segs) == 0 {
		return ""
	}
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = sanitizeLabel(s)
	}
	return strings.Join(out, "/")
}

// commonDirSegments returns the unsanitized path segments of the longest
// directory shared by all files. Returns nil when files share no common
// directory or when any file is at the root.
func commonDirSegments(files []PendingFile) []string {
	if len(files) == 0 {
		return nil
	}
	dirParts := func(p string) []string {
		segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
		if len(segs) <= 1 {
			return nil
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
	return common
}

// commonDirLabel finds the longest directory path shared by all files,
// sanitizes it (slashes → underscores), and returns it. Returns "_root"
// when files share no common directory.
func commonDirLabel(files []PendingFile) string {
	common := commonDirSegments(files)
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
// the final zip size and the list of entry paths written. wrap, when
// non-nil, is applied once per source reader before bytes flow into
// the zip writer; the engine uses it to inject a counterReader that
// emits live copy_progress events.
func CreateZip(ctx context.Context, src source.Source, files []PendingFile, outPath string, wrap func(io.Reader) io.Reader) (size int64, entries []string, err error) {
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
		if err := writeZipEntry(ctx, zw, src, f, wrap); err != nil {
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

func writeZipEntry(ctx context.Context, zw *zip.Writer, src source.Source, f PendingFile, wrap func(io.Reader) io.Reader) error {
	w, err := zw.Create(f.RelPath)
	if err != nil {
		return err
	}
	rc, err := src.Open(ctx, f.RelPath)
	if err != nil {
		return err
	}
	defer rc.Close()
	var reader io.Reader = rc
	if wrap != nil {
		reader = wrap(rc)
	}
	// Same ctx-aware shim as copyAndHash so /api/cancel exits a multi-GB
	// zip-entry read within one io.Copy buffer instead of waiting for
	// the file to fully drain into the zip writer.
	reader = &ctxReader{ctx: ctx, r: reader}
	_, err = io.Copy(w, reader)
	return err
}
