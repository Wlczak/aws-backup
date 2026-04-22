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

	// Threshold 3 -> photos qualifies, docs does not, root does not.
	groups := GroupFiles(files, 3)
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

func TestCreateZipRoundTrip(t *testing.T) {
	root := t.TempDir()
	payload := map[string]string{
		"photos/a.jpg":      "alphaalphaalpha",
		"photos/b.jpg":      "bravo",
		"photos/sub/c.jpg":  "charlie-deepcontent",
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
	size, entries, err := CreateZip(context.Background(), src, pending, out)
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
		filepath.Join(t.TempDir(), "out.zip"))
	if err == nil {
		t.Fatal("expected cancel error")
	}
}
