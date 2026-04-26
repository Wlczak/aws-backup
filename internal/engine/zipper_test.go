package engine

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wlczak/aws-backup/internal/source"
)

func TestGroupFiles(t *testing.T) {
	files := []PendingFile{
		{ID: 1, RelPath: "root.txt"},
		{ID: 2, RelPath: "photos/a.jpg"},
		{ID: 3, RelPath: "photos/b.jpg"},
		{ID: 4, RelPath: "photos/c.jpg"},
		{ID: 5, RelPath: "docs/x.pdf"},
		{ID: 6, RelPath: "docs/sub/y.pdf"},
	}

	// Threshold 3, no size cap -> photos qualifies, docs does not, root does not.
	groups := GroupFiles(files, 3, 0, 0)
	if len(groups) != 3 {
		t.Fatalf("want 3 groups, got %d", len(groups))
	}
	byTop := map[string]Group{}
	for _, g := range groups {
		byTop[g.TopDir] = g
	}
	if !byTop["photos"].Zip {
		t.Error("photos should be zipped (3 files, threshold 3)")
	}
	if byTop["docs"].Zip {
		t.Error("docs should not be zipped (2 files, threshold 3)")
	}
	if byTop[""].Zip {
		t.Error("root should not be zipped (1 file)")
	}
	if len(byTop["photos"].Files) != 3 || len(byTop["docs"].Files) != 2 {
		t.Errorf("unexpected file counts: %+v", byTop)
	}
}

func TestGroupFilesSizeCap(t *testing.T) {
	// Two subdirs under "media/", each well under the cap individually
	// but over the cap together. Expect one group per subdir, not one
	// monolithic "media" group.
	files := []PendingFile{
		{ID: 1, RelPath: "media/2024/a.mov", Size: 800},
		{ID: 2, RelPath: "media/2024/b.mov", Size: 800},
		{ID: 3, RelPath: "media/2025/c.mov", Size: 800},
		{ID: 4, RelPath: "media/2025/d.mov", Size: 800},
	}
	// threshold 2, cap 2000 bytes -> 2024 = 1600B, 2025 = 1600B, both
	// under cap as subtrees; media/ together = 3200B, over cap.
	groups := GroupFiles(files, 2, 0, 2000)
	if len(groups) != 2 {
		t.Fatalf("want 2 groups (one per year), got %d: %+v", len(groups), groups)
	}
	for _, g := range groups {
		if !g.Zip {
			t.Errorf("group %q should be zipped", g.TopDir)
		}
		if g.TopDir != "media/2024" && g.TopDir != "media/2025" {
			t.Errorf("unexpected TopDir %q — grouping should descend into subdirs", g.TopDir)
		}
	}
}

func TestGroupFilesSizeCapChunksLooseFiles(t *testing.T) {
	// All files directly under one directory, collectively over the cap.
	// Cannot split by subdir (there are none), so chunk loose files by
	// cumulative size. 6 × 500B under a 1200B cap -> 3 chunks of ~1000B.
	files := []PendingFile{
		{ID: 1, RelPath: "bulk/a", Size: 500},
		{ID: 2, RelPath: "bulk/b", Size: 500},
		{ID: 3, RelPath: "bulk/c", Size: 500},
		{ID: 4, RelPath: "bulk/d", Size: 500},
		{ID: 5, RelPath: "bulk/e", Size: 500},
		{ID: 6, RelPath: "bulk/f", Size: 500},
	}
	groups := GroupFiles(files, 2, 0, 1200)
	if len(groups) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(groups))
	}
	total := 0
	for _, g := range groups {
		if !g.Zip {
			t.Errorf("chunked group should be zipped: %+v", g)
		}
		total += len(g.Files)
	}
	if total != len(files) {
		t.Errorf("chunks lost files: covered=%d want=%d", total, len(files))
	}
}

func TestGroupFilesSingleFileOverCap(t *testing.T) {
	// A single file larger than the cap cannot be split — should fall
	// back to individual upload, not a solo-zip wrapper.
	files := []PendingFile{
		{ID: 1, RelPath: "huge/one.bin", Size: 10_000_000_000}, // 10 GB
		{ID: 2, RelPath: "huge/two.bin", Size: 100},
	}
	groups := GroupFiles(files, 2, 0, 1<<30) // 1 GB cap
	var haveIndividual bool
	for _, g := range groups {
		for _, f := range g.Files {
			if f.ID == 1 && !g.Zip {
				haveIndividual = true
			}
		}
	}
	if !haveIndividual {
		t.Errorf("oversized file should land in an individual (non-zipped) group: %+v", groups)
	}
}

func TestGroupFilesMinZipDirFiles(t *testing.T) {
	// media/ exceeds the 2000-byte cap, so it would normally split into
	// three subdirs. With minZipDirFiles=3, the two small subdirs (2 files
	// each) are folded back into media/'s loose pool and chunked together,
	// while the large subdir (4 files) becomes its own group.
	files := []PendingFile{
		{ID: 1, RelPath: "media/small1/a.mov", Size: 400},
		{ID: 2, RelPath: "media/small1/b.mov", Size: 400},
		{ID: 3, RelPath: "media/small2/c.mov", Size: 400},
		{ID: 4, RelPath: "media/small2/d.mov", Size: 400},
		{ID: 5, RelPath: "media/big/e.mov", Size: 600},
		{ID: 6, RelPath: "media/big/f.mov", Size: 600},
		{ID: 7, RelPath: "media/big/g.mov", Size: 600},
		{ID: 8, RelPath: "media/big/h.mov", Size: 600},
	}
	// Total = 4000B > cap=2000B; big/=2400B > cap; small subdirs=800B each.
	// minZipDirFiles=3: small1 (2 files) and small2 (2 files) get folded.
	groups := GroupFiles(files, 2, 3, 2000)

	byTop := map[string][]PendingFile{}
	for _, g := range groups {
		byTop[g.TopDir] = append(byTop[g.TopDir], g.Files...)
	}

	// big/ should be its own group (4 files >= 3).
	if len(byTop["media/big"]) != 4 {
		t.Errorf("media/big: want 4 files, got %d (groups=%+v)", len(byTop["media/big"]), groups)
	}
	// small1/ and small2/ should be folded into media/ loose files —
	// they must NOT appear as their own groups.
	if _, ok := byTop["media/small1"]; ok {
		t.Error("media/small1 should be folded, not its own group")
	}
	if _, ok := byTop["media/small2"]; ok {
		t.Error("media/small2 should be folded, not its own group")
	}
	// All 4 folded files (ids 1-4) must appear somewhere under media/.
	mediaFiles := map[int64]bool{}
	for _, g := range groups {
		if g.TopDir == "media" || g.TopDir == "" {
			for _, f := range g.Files {
				mediaFiles[f.ID] = true
			}
		}
	}
	for _, id := range []int64{1, 2, 3, 4} {
		if !mediaFiles[id] {
			t.Errorf("folded file id=%d missing from media loose groups", id)
		}
	}
}

func TestZipName(t *testing.T) {
	// Files all in photos/ → label is "photos"
	photosFiles := []PendingFile{
		{RelPath: "photos/a.jpg"},
		{RelPath: "photos/b.jpg"},
		{RelPath: "photos/c.jpg"},
	}
	if got := ZipName(photosFiles, 1); got != "photos_1.zip" {
		t.Errorf("photos: got %q", got)
	}

	// Files in a nested common dir → label reflects full hierarchy
	nestedFiles := []PendingFile{
		{RelPath: "backup/folder1/images/1.jpg"},
		{RelPath: "backup/folder1/images/2.jpg"},
		{RelPath: "backup/folder1/images/3.jpg"},
	}
	if got := ZipName(nestedFiles, 2); got != "backup_folder1_images_2.zip" {
		t.Errorf("nested: got %q", got)
	}

	// Mixed sub-dirs under same parent → common dir is the parent
	mixedFiles := []PendingFile{
		{RelPath: "backup/folder1/images/1.jpg"},
		{RelPath: "backup/folder1/docs/a.pdf"},
	}
	if got := ZipName(mixedFiles, 1); got != "backup_folder1_1.zip" {
		t.Errorf("mixed: got %q", got)
	}

	// Root-level files → _root label
	rootFiles := []PendingFile{
		{RelPath: "a.txt"},
		{RelPath: "b.txt"},
	}
	if got := ZipName(rootFiles, 3); got != "_root_3.zip" {
		t.Errorf("root: got %q", got)
	}

	// Names with unsafe characters → sanitized
	uglyFiles := []PendingFile{
		{RelPath: "weird name with spaces!/a.txt"},
		{RelPath: "weird name with spaces!/b.txt"},
	}
	if got := ZipName(uglyFiles, 1); got != "weird_name_with_spaces__1.zip" {
		t.Errorf("sanitized: got %q", got)
	}
}

func TestZipRelPath(t *testing.T) {
	cases := []struct {
		name  string
		files []PendingFile
		n     int
		want  string
	}{
		{
			name: "single common dir",
			files: []PendingFile{
				{RelPath: "photos/a.jpg"},
				{RelPath: "photos/b.jpg"},
			},
			n:    1,
			want: "photos/photos_1.zip",
		},
		{
			name: "nested common dir",
			files: []PendingFile{
				{RelPath: "backup/folder1/images/1.jpg"},
				{RelPath: "backup/folder1/images/2.jpg"},
			},
			n:    2,
			want: "backup/folder1/images/backup_folder1_images_2.zip",
		},
		{
			name: "mixed siblings share parent",
			files: []PendingFile{
				{RelPath: "backup/folder1/images/1.jpg"},
				{RelPath: "backup/folder1/docs/a.pdf"},
			},
			n:    1,
			want: "backup/folder1/backup_folder1_1.zip",
		},
		{
			name: "root-level files have no prefix",
			files: []PendingFile{
				{RelPath: "a.txt"},
				{RelPath: "b.txt"},
			},
			n:    3,
			want: "_root_3.zip",
		},
		{
			name: "no common ancestor → root",
			files: []PendingFile{
				{RelPath: "alpha/a.txt"},
				{RelPath: "beta/b.txt"},
			},
			n:    1,
			want: "_root_1.zip",
		},
		{
			name: "unsafe chars sanitized per-segment (slashes preserved)",
			files: []PendingFile{
				{RelPath: "weird name!/sub/a.txt"},
				{RelPath: "weird name!/sub/b.txt"},
			},
			n:    1,
			want: "weird_name_/sub/weird_name__sub_1.zip",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ZipRelPath(tc.files, tc.n); got != tc.want {
				t.Errorf("ZipRelPath: got %q want %q", got, tc.want)
			}
		})
	}
}

func TestCreateZipRoundTrip(t *testing.T) {
	root := t.TempDir()
	payload := map[string]string{
		"photos/a.jpg":     "alphaalphaalpha",
		"photos/b.jpg":     "bravo",
		"photos/sub/c.jpg": "charlie-deepcontent",
	}
	for rel, body := range payload {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	src, err := source.NewLocalDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	pending := []PendingFile{
		{ID: 1, RelPath: "photos/a.jpg"},
		{ID: 2, RelPath: "photos/b.jpg"},
		{ID: 3, RelPath: "photos/sub/c.jpg"},
	}

	out := filepath.Join(t.TempDir(), "photos.zip")
	size, entries, err := CreateZip(context.Background(), src, pending, out, nil)
	if err != nil {
		t.Fatalf("CreateZip: %v", err)
	}
	if size <= 0 {
		t.Fatal("size should be > 0")
	}
	if len(entries) != len(pending) {
		t.Errorf("entries=%d want %d", len(entries), len(pending))
	}

	// Reopen the zip and verify contents.
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	got := map[string]string{}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		got[zf.Name] = string(b)
	}
	for k, want := range payload {
		if got[k] != want {
			t.Errorf("entry %s: got %q want %q", k, got[k], want)
		}
	}
}

func TestParseZipNumber(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"photos_1.zip", 1},
		{"photos/photos_1.zip", 1},
		{"photos/2024/photos_2024_3.zip", 3},
		{"_root_7.zip", 7},
		{"nonum.zip", 0},
		{"", 0},
		{"nodot", 0},
	}
	for _, tc := range cases {
		if got := parseZipNumber(tc.in); got != tc.want {
			t.Errorf("parseZipNumber(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestCreateZipCancel(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, _ := source.NewLocalDir(root)
	defer src.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := CreateZip(ctx, src, []PendingFile{{RelPath: "a.txt"}},
		filepath.Join(t.TempDir(), "out.zip"), nil)
	if err == nil {
		t.Fatal("expected cancel error")
	}
}
