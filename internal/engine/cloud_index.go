package engine

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"github.com/Wlczak/aws-backup/internal/pathutil"
	"github.com/Wlczak/aws-backup/internal/storage"
)

// CloudFile records where a given source-relative path lives in S3: either
// inside a zip archive (ZipKey set, with the zip's `.index.txt` listing the
// path) or as a standalone object (S3Key set). Exactly one of the two is
// populated.
type CloudFile struct {
	Path   string
	ZipKey string
	S3Key  string
}

// CloudIndex is the set of source-relative paths currently backed up to
// S3, assembled from every `.index.txt` sidecar plus every standalone
// file's S3 key. Lookups are keyed by source-relative path.
type CloudIndex struct {
	Files      map[string]CloudFile
	IndexCount int // number of .index.txt sidecars consumed
	ZipCount   int // distinct zip archives referenced
	Standalone int // standalone entries
}

// LoadCloudIndex walks every object under prefix, downloads the per-zip
// `.index.txt` sidecars, and combines their entries with standalone S3
// objects into a single path → location map. `prefix` MUST match the
// engine's KeyPrefix so standalone keys round-trip to their source-relative
// path.
func LoadCloudIndex(ctx context.Context, s storage.Storage, prefix string, standaloneKeys []string) (CloudIndex, error) {
	idx := CloudIndex{Files: map[string]CloudFile{}}
	if s == nil {
		return idx, fmt.Errorf("storage is nil")
	}

	listPrefix := pathutil.NormalizeS3ListPrefix(prefix)
	keys, err := s.List(ctx, listPrefix)
	if err != nil {
		return idx, fmt.Errorf("list %q: %w", listPrefix, err)
	}

	// Use the listing as ground truth for "present in cloud" — the DB's
	// standaloneKeys list may include rows for files whose S3 object has
	// been deleted, and those should NOT count as covered.
	inS3 := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		inS3[k] = struct{}{}
	}

	for _, key := range keys {
		if !strings.HasSuffix(key, ZipIndexSuffix) {
			continue
		}
		zipKey := strings.TrimSuffix(key, ZipIndexSuffix)
		// Only consume sidecars whose backing zip is also present. A
		// dangling .index.txt (e.g. zip manually deleted from S3) would
		// otherwise inflate the cloud index with phantom paths.
		if _, ok := inS3[zipKey]; !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return idx, err
		}
		if err := readIndexInto(ctx, s, key, zipKey, &idx); err != nil {
			return idx, fmt.Errorf("read %s: %w", key, err)
		}
		idx.IndexCount++
		idx.ZipCount++
	}

	addStandalone := func(key string) {
		if !strings.HasPrefix(key, listPrefix) {
			return
		}
		if strings.HasSuffix(key, ZipIndexSuffix) {
			return
		}
		// If a zip's sidecar is present, the zip key is represented through
		// the indexed member list instead of as a standalone object.
		if _, ok := inS3[key+ZipIndexSuffix]; ok {
			return
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(key, prefix), "/")
		if rel == "" {
			return
		}
		if _, seen := idx.Files[rel]; seen {
			return
		}
		idx.Files[rel] = CloudFile{Path: rel, S3Key: key}
		idx.Standalone++
	}

	for _, key := range keys {
		addStandalone(key)
	}
	for _, key := range standaloneKeys {
		addStandalone(key)
	}

	return idx, nil
}

func readIndexInto(ctx context.Context, s storage.Storage, indexKey, zipKey string, idx *CloudIndex) error {
	rc, err := s.Get(ctx, indexKey)
	if err != nil {
		return err
	}
	defer rc.Close()

	sc := bufio.NewScanner(rc)
	// Allow long paths — default token size (64 KiB) is already plenty, but
	// be explicit for readability.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if _, seen := idx.Files[line]; seen {
			// Same file indexed in multiple zips (e.g. re-uploaded version)
			// — keep the first occurrence; caller treats "present in cloud"
			// as a boolean.
			continue
		}
		idx.Files[line] = CloudFile{Path: line, ZipKey: zipKey}
	}
	return sc.Err()
}
