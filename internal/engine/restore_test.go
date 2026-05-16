package engine

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/storage"
)

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

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
	if err := d.MarkUploadedBatch(ctx, []int64{ids["notes.txt"]}, md5hex("hello"), "backups/notes.txt", now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestored(ctx, "backups/notes.txt", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := d.SetZipName(ctx, []int64{ids["photos/a.jpg"], ids["photos/b.jpg"]}, "photos/photos_1.zip"); err != nil {
		t.Fatal(err)
	}
	// MarkUploadedBatch sets the same md5 on every id; per-file md5s are
	// fine for this test because each file's bytes are identical-length
	// distinct content (the verifier only fires when md5 disagrees with
	// the bytes actually written, not within the batch).
	if err := d.MarkUploadedBatch(ctx, []int64{ids["photos/a.jpg"]}, md5hex("aaa"),
		"backups/photos/photos_1.zip", now); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{ids["photos/b.jpg"]}, md5hex("bbbb"),
		"backups/photos/photos_1.zip", now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestored(ctx, "backups/photos/photos_1.zip", now.Add(24*time.Hour)); err != nil {
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

func TestDownloadMirrorToDirZipMissingMember(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	seed := []db.BatchEntry{
		{Path: "photos/a.jpg", Size: 3, ModTime: now},
		{Path: "photos/b.jpg", Size: 4, ModTime: now},
	}
	res, err := d.UpsertFileBatch(ctx, seed, now)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	ids := []int64{res[0].ID, res[1].ID}
	if err := d.MarkZipUploadedBatch(ctx, db.Zip{
		ZipName:    "photos/photos_1.zip",
		Size:       1234,
		MD5:        "archive-md5",
		SHA256:     "archive-sha",
		S3Key:      "backups/photos/photos_1.zip",
		UploadedAt: &now,
		LastSeenAt: now,
	}, []db.ZipMemberUpload{
		{ID: ids[0], MD5: md5hex("aaa")},
		{ID: ids[1], MD5: md5hex("bbbb")},
	}); err != nil {
		t.Fatalf("MarkZipUploadedBatch: %v", err)
	}

	mirror := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mirror, "photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "photos", "a.jpg"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := storage.NewMemStorage()
	zipBytes := buildZip(t, map[string]string{
		"photos/a.jpg": "aaa",
		"photos/b.jpg": "bbbb",
	})
	if _, err := store.Put(ctx, "backups/photos/photos_1.zip", bytes.NewReader(zipBytes), int64(len(zipBytes))); err != nil {
		t.Fatal(err)
	}

	stats, err := DownloadMirrorToDir(ctx, DownloadOptions{
		DB:          d,
		Storage:     store,
		KeyPrefix:   "backups/",
		DownloadDir: mirror,
		TmpDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("DownloadMirrorToDir: %v", err)
	}
	if stats.Scanned != 2 || stats.Present != 1 || stats.Missing != 1 {
		t.Fatalf("unexpected scan stats: %+v", stats)
	}
	if stats.ObjectCount != 1 {
		t.Fatalf("ObjectCount = %d want 1", stats.ObjectCount)
	}
	if stats.FilesWritten != 1 {
		t.Fatalf("FilesWritten = %d want 1", stats.FilesWritten)
	}

	got, err := os.ReadFile(filepath.Join(mirror, "photos", "b.jpg"))
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(got) != "bbbb" {
		t.Fatalf("downloaded file = %q want %q", got, "bbbb")
	}

	files, _, err := d.ListFiles(ctx, db.FilesFilter{})
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	for _, f := range files {
		switch f.Path {
		case "photos/a.jpg":
			if !f.DownloadPresent {
				t.Fatalf("expected %s to remain marked present", f.Path)
			}
		case "photos/b.jpg":
			if !f.DownloadPresent {
				t.Fatalf("expected %s to be marked present after download", f.Path)
			}
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
	if err := d.MarkUploadedBatch(ctx, []int64{ids["photos/a.jpg"]}, md5hex("aaa"), "backups/photos/a.jpg", now); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{ids["docs/readme.md"]}, md5hex("docs!"), "backups/docs/readme.md", now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestored(ctx, "backups/photos/a.jpg", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestored(ctx, "backups/docs/readme.md", now.Add(24*time.Hour)); err != nil {
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

// TestRestoreToDirChecksumMismatch verifies that restore detects bytes
// that don't match the DB-recorded md5 (simulating S3 bit-rot, a
// tampered endpoint, or local-disk corruption) and surfaces them in
// stats.Errors instead of silently writing a corrupt file. (#75)
func TestRestoreToDirChecksumMismatch(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now()
	res, _ := d.UpsertFileBatch(ctx, []db.BatchEntry{{Path: "notes.txt", Size: 5, ModTime: now}}, now)
	id := res[0].ID
	// Record an md5 that does NOT match what's in storage.
	wrongMD5 := md5hex("not-the-real-content")
	if err := d.MarkUploadedBatch(ctx, []int64{id}, wrongMD5, "backups/notes.txt", now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestored(ctx, "backups/notes.txt", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	store := storage.NewMemStorage()
	mustPut(t, store, "backups/notes.txt", "hello")

	target := t.TempDir()
	stats, err := RestoreToDir(ctx, RestoreOptions{
		DB: d, Storage: store, KeyPrefix: "backups/", TargetDir: target,
		Paths: []string{"/"},
	})
	if err != nil {
		t.Fatalf("RestoreToDir: %v", err)
	}
	if stats.FilesWritten != 0 {
		t.Errorf("FilesWritten: got %d want 0 on mismatch", stats.FilesWritten)
	}
	if len(stats.Errors) == 0 || !strings.Contains(stats.Errors[0], "checksum mismatch") {
		t.Errorf("expected checksum-mismatch error, got %v", stats.Errors)
	}

	// SkipChecksum opt-out path: same setup, mismatch tolerated.
	stats2, err := RestoreToDir(ctx, RestoreOptions{
		DB: d, Storage: store, KeyPrefix: "backups/", TargetDir: t.TempDir(),
		Paths: []string{"/"}, SkipChecksum: true,
	})
	if err != nil {
		t.Fatalf("RestoreToDir SkipChecksum: %v", err)
	}
	if stats2.FilesWritten != 1 {
		t.Errorf("SkipChecksum: FilesWritten got %d want 1", stats2.FilesWritten)
	}
}

func TestRestoreToDirEmitsProgress(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now()
	res, err := d.UpsertFileBatch(ctx, []db.BatchEntry{{Path: "notes.txt", Size: 5, ModTime: now}}, now)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := d.MarkUploaded(ctx, res[0].ID, md5hex("hello"), "backups/notes.txt", now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestored(ctx, "backups/notes.txt", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	store := storage.NewMemStorage()
	mustPut(t, store, "backups/notes.txt", "hello")

	var events []Event
	stats, err := RestoreToDir(ctx, RestoreOptions{
		DB:        d,
		Storage:   store,
		KeyPrefix: "backups/",
		TargetDir: t.TempDir(),
		Paths:     []string{"/"},
		Emit: func(ev Event) {
			events = append(events, ev)
		},
	})
	if err != nil {
		t.Fatalf("RestoreToDir: %v", err)
	}
	if stats.FilesWritten != 1 {
		t.Fatalf("FilesWritten = %d want 1", stats.FilesWritten)
	}
	if len(events) < 4 {
		t.Fatalf("event count = %d want at least 4 (%+v)", len(events), events)
	}
	if events[0].Type != EventRestoreDownloadStart {
		t.Fatalf("start type = %q", events[0].Type)
	}
	if got := events[0].Data["total"].(int); got != 1 {
		t.Fatalf("start total = %d want 1", got)
	}
	if events[1].Type != EventRestoreDownloadProgress {
		t.Fatalf("progress type = %q", events[1].Type)
	}
	if got := events[1].Data["processed"].(int); got != 1 {
		t.Fatalf("progress processed = %d want 1", got)
	}
	if got := events[1].Data["current_total_bytes"].(int64); got != 5 {
		t.Fatalf("progress current_total_bytes = %d want 5", got)
	}
	if got := events[1].Data["current_percent"].(int); got != 0 {
		t.Fatalf("progress current_percent = %d want 0", got)
	}
	if events[len(events)-1].Type != EventRestoreDownloadComplete {
		t.Fatalf("complete type = %q", events[len(events)-1].Type)
	}
	if got := events[len(events)-1].Data["errors"].(int); got != 0 {
		t.Fatalf("complete errors = %d want 0", got)
	}
}

func TestRestoreToDirEmitsByteLevelProgress(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now()
	res, err := d.UpsertFileBatch(ctx, []db.BatchEntry{{Path: "big.bin", Size: 5, ModTime: now}}, now)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := d.MarkUploaded(ctx, res[0].ID, md5hex("abcde"), "backups/big.bin", now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestored(ctx, "backups/big.bin", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	store := storage.NewMemStorage()
	mustPut(t, store, "backups/big.bin", "abcde")
	slow := &slowGetStorage{Storage: store}

	var events []Event
	_, err = RestoreToDir(ctx, RestoreOptions{
		DB:        d,
		Storage:   slow,
		KeyPrefix: "backups/",
		TargetDir: t.TempDir(),
		Paths:     []string{"/"},
		Emit: func(ev Event) {
			events = append(events, ev)
		},
	})
	if err != nil {
		t.Fatalf("RestoreToDir: %v", err)
	}

	var sawMidFile bool
	for _, ev := range events {
		if ev.Type != EventRestoreDownloadProgress {
			continue
		}
		read := ev.Data["current_bytes"].(int64)
		total := ev.Data["current_total_bytes"].(int64)
		if read > 0 && read < total {
			sawMidFile = true
			if pct := ev.Data["current_percent"].(int); pct <= 0 || pct >= 100 {
				t.Fatalf("expected in-flight percent to be between 0 and 100, got %d", pct)
			}
			break
		}
	}
	if !sawMidFile {
		t.Fatalf("expected a mid-file progress event, got %+v", events)
	}
}

type slowGetStorage struct {
	storage.Storage
}

func (s *slowGetStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.Storage.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return &slowReadCloser{ReadCloser: rc}, nil
}

type slowReadCloser struct {
	io.ReadCloser
}

func (s *slowReadCloser) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	buf := make([]byte, 1)
	n, err := s.ReadCloser.Read(buf)
	if n > 0 {
		p[0] = buf[0]
		time.Sleep(300 * time.Millisecond)
		return 1, nil
	}
	return 0, err
}

func TestRestoreToDirSkipsNonRestoredFiles(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now()
	res, err := d.UpsertFileBatch(ctx, []db.BatchEntry{
		{Path: "ready.txt", Size: 5, ModTime: now},
		{Path: "thawing.txt", Size: 7, ModTime: now},
		{Path: "idle.txt", Size: 9, ModTime: now},
	}, now)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := d.MarkUploaded(ctx, res[0].ID, md5hex("ready"), "backups/ready.txt", now); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploaded(ctx, res[1].ID, md5hex("thawing"), "backups/thawing.txt", now); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploaded(ctx, res[2].ID, md5hex("idle"), "backups/idle.txt", now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestored(ctx, "backups/ready.txt", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestoreInProgress(ctx, "backups/thawing.txt"); err != nil {
		t.Fatal(err)
	}

	store := storage.NewMemStorage()
	mustPut(t, store, "backups/ready.txt", "ready")
	mustPut(t, store, "backups/thawing.txt", "thawing")
	mustPut(t, store, "backups/idle.txt", "idle")

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
	if stats.FilesWritten != 1 {
		t.Fatalf("FilesWritten = %d want 1 (%+v)", stats.FilesWritten, stats)
	}
	if len(stats.Skipped) != 2 {
		t.Fatalf("Skipped = %v want 2 entries", stats.Skipped)
	}
	if _, err := os.Stat(filepath.Join(target, "ready.txt")); err != nil {
		t.Fatalf("ready.txt missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "thawing.txt")); !os.IsNotExist(err) {
		t.Fatalf("thawing.txt should not have been restored, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "idle.txt")); !os.IsNotExist(err) {
		t.Fatalf("idle.txt should not have been restored, err=%v", err)
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
