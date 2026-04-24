package engine

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/storage"
)

func TestRestoreToDirMixed(t *testing.T) {
	ctx := context.Background()

	// DB: one standalone file and two zipped files.
	d := openTestDB(t)
	now := time.Now()
	seed := []db.BatchEntry{
		{Path: "notes.txt", Size: 5, ModTime: now},
		{Path: "photos/a.jpg", Size: 3, ModTime: now},
		{Path: "photos/b.jpg", Size: 4, ModTime: now},
	}
	res, err := d.UpsertFileBatch(ctx, seed, now)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	ids := make(map[string]int64)
	for i, r := range res {
		ids[seed[i].Path] = r.ID
	}
	if err := d.MarkUploadedBatch(ctx, []int64{ids["notes.txt"]}, "md5", "backups/notes.txt", now); err != nil {
		t.Fatal(err)
	}
	if err := d.SetZipName(ctx, []int64{ids["photos/a.jpg"], ids["photos/b.jpg"]}, "photos/photos_1.zip"); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{ids["photos/a.jpg"], ids["photos/b.jpg"]}, "md5",
		"backups/photos/photos_1.zip", now); err != nil {
		t.Fatal(err)
	}

	// Storage: put standalone body + a real zip covering the two photos.
	store := storage.NewMemStorage()
	mustPut(t, store, "backups/notes.txt", "hello")

	zipBytes := buildZip(t, map[string]string{
		"photos/a.jpg": "aaa",
		"photos/b.jpg": "bbbb",
	})
	if _, err := store.Put(ctx, "backups/photos/photos_1.zip", bytes.NewReader(zipBytes), int64(len(zipBytes))); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	stats, err := RestoreToDir(ctx, RestoreOptions{
		DB:        d,
		Storage:   store,
		KeyPrefix: "backups/",
		TargetDir: target,
		Paths:     []string{"/"},
	})
	if err != nil {
		t.Fatalf("RestoreToDir: %v", err)
	}
	if stats.FilesWritten != 3 {
		t.Errorf("FilesWritten: got %d want 3 (%+v)", stats.FilesWritten, stats)
	}
	if stats.BytesWritten != int64(len("hello")+len("aaa")+len("bbbb")) {
		t.Errorf("BytesWritten: got %d (%+v)", stats.BytesWritten, stats)
	}

	expect := map[string]string{
		"notes.txt":    "hello",
		"photos/a.jpg": "aaa",
		"photos/b.jpg": "bbbb",
	}
	for rel, want := range expect {
		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s: got %q want %q", rel, got, want)
		}
	}
}

func TestRestoreToDirPrefixFilter(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now()
	seed := []db.BatchEntry{
		{Path: "photos/a.jpg", Size: 3, ModTime: now},
		{Path: "docs/readme.md", Size: 5, ModTime: now},
	}
	res, _ := d.UpsertFileBatch(ctx, seed, now)
	ids := map[string]int64{seed[0].Path: res[0].ID, seed[1].Path: res[1].ID}
	if err := d.MarkUploadedBatch(ctx, []int64{ids["photos/a.jpg"]}, "m", "backups/photos/a.jpg", now); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{ids["docs/readme.md"]}, "m", "backups/docs/readme.md", now); err != nil {
		t.Fatal(err)
	}

	store := storage.NewMemStorage()
	mustPut(t, store, "backups/photos/a.jpg", "aaa")
	mustPut(t, store, "backups/docs/readme.md", "docs!")

	target := t.TempDir()
	stats, err := RestoreToDir(ctx, RestoreOptions{
		DB: d, Storage: store, KeyPrefix: "backups/", TargetDir: target,
		Paths: []string{"photos"},
	})
	if err != nil {
		t.Fatalf("RestoreToDir: %v", err)
	}
	if stats.FilesWritten != 1 {
		t.Errorf("expected only photos to restore, got %d (%+v)", stats.FilesWritten, stats)
	}
	if _, err := os.Stat(filepath.Join(target, "docs", "readme.md")); !os.IsNotExist(err) {
		t.Errorf("docs/readme.md should NOT have been restored, got err=%v", err)
	}
}

func TestRestoreToDirUnknownPath(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	store := storage.NewMemStorage()

	stats, err := RestoreToDir(ctx, RestoreOptions{
		DB: d, Storage: store, KeyPrefix: "backups/", TargetDir: t.TempDir(),
		Paths: []string{"nope/absent"},
	})
	if err != nil {
		t.Fatalf("RestoreToDir: %v", err)
	}
	if stats.FilesWritten != 0 {
		t.Errorf("expected 0 written, got %d", stats.FilesWritten)
	}
	if len(stats.Skipped) != 1 || !strings.Contains(stats.Skipped[0], "nope/absent") {
		t.Errorf("expected Skipped to mention nope/absent, got %v", stats.Skipped)
	}
}

func TestRestoreToDirRejectsNonAbsTarget(t *testing.T) {
	d := openTestDB(t)
	_, err := RestoreToDir(context.Background(), RestoreOptions{
		DB: d, Storage: storage.NewMemStorage(), TargetDir: "relative/path",
	})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("expected absolute-path error, got %v", err)
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	bad := []string{
		"../etc/passwd",
		"../../evil",
		"foo/../../etc/passwd",
		"/absolute/elsewhere",
		"",
	}
	for _, p := range bad {
		if _, err := safeJoin(root, p); err == nil {
			t.Errorf("safeJoin(%q) should have rejected the path", p)
		}
	}

	good := []string{
		"a.txt",
		"photos/2024/a.jpg",
		"deep/nested/dir/file.bin",
	}
	for _, p := range good {
		abs, err := safeJoin(root, p)
		if err != nil {
			t.Errorf("safeJoin(%q) unexpected error: %v", p, err)
			continue
		}
		if !strings.HasPrefix(abs, root) {
			t.Errorf("safeJoin(%q) = %q does not stay under %q", p, abs, root)
		}
	}
}

// --- helpers ---

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "restore.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := io.Copy(w, strings.NewReader(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}
