package engine

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/source"
	"github.com/Wlczak/aws-backup/internal/storage"
)

type collector struct {
	mu     sync.Mutex
	events []Event
}

func (c *collector) emit(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *collector) byType(t string) []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Event
	for _, e := range c.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func newTestEngine(t *testing.T, threshold int) (*Engine, *db.DB, *source.LocalDir, *storage.MemStorage, string, *collector) {
	t.Helper()
	root := t.TempDir()
	tmp := t.TempDir()
	src, err := source.NewLocalDir(root)
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemStorage()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "engine.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); src.Close() })

	col := &collector{}
	eng := New(Options{
		DB:             d,
		Source:         src,
		Storage:        store,
		TmpDir:         tmp,
		KeyPrefix:      "backups",
		ChunkSize:      2,
		ZipThresh:      threshold,
		EnableZipIndex: true,
		Emit:           col.emit,
	})
	return eng, d, src, store, root, col
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEngineHappyPathMixedGroups(t *testing.T) {
	eng, d, _, store, root, col := newTestEngine(t, 3)

	// photos/ has 3 files -> zipped. docs/ has 2 -> individual. root.txt -> individual.
	writeFile(t, root, "photos/a.jpg", "alpha")
	writeFile(t, root, "photos/b.jpg", "bravo")
	writeFile(t, root, "photos/c.jpg", "charlie")
	writeFile(t, root, "docs/x.pdf", "document x")
	writeFile(t, root, "docs/y.pdf", "document y")
	writeFile(t, root, "top.txt", "top level")

	ctx := context.Background()
	runID, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, _ := d.GetRun(ctx, runID)
	if run.Status != db.RunCompleted {
		t.Fatalf("status=%q want completed", run.Status)
	}
	if run.FilesScanned != 6 {
		t.Errorf("scanned=%d want 6", run.FilesScanned)
	}
	if run.FilesUploaded != 6 {
		t.Errorf("uploaded=%d want 6", run.FilesUploaded)
	}

	// 4 uploads: 1 zip + 2 individual docs + 1 individual top.
	starts := col.byType(EventUploadStart)
	completes := col.byType(EventUploadComplete)
	if len(starts) != 4 || len(completes) != 4 {
		t.Errorf("want 4 starts/4 completes, got %d/%d", len(starts), len(completes))
	}

	// Exactly one zip object in storage, one matching .index.txt sidecar,
	// and the rest are per-file keys.
	keys := store.Keys()
	var zipCount, idxCount, indCount int
	for _, k := range keys {
		switch {
		case filepath.Ext(k) == ".zip":
			zipCount++
		case strings.HasSuffix(k, ".zip.index.txt"):
			idxCount++
		default:
			indCount++
		}
	}
	if zipCount != 1 {
		t.Errorf("want 1 zip, got %d", zipCount)
	}
	if idxCount != 1 {
		t.Errorf("want 1 zip index sidecar, got %d", idxCount)
	}
	if indCount != 3 { // docs/x, docs/y, top.txt
		t.Errorf("want 3 individual uploads, got %d", indCount)
	}

	// DB state: all 6 files should be 'uploaded' with s3_key set.
	files, _, _ := d.ListFiles(ctx, db.FilesFilter{})
	for _, f := range files {
		if f.Status != db.StatusUploaded {
			t.Errorf("%s status=%q", f.Path, f.Status)
		}
		if f.S3Key == "" {
			t.Errorf("%s missing s3_key", f.Path)
		}
	}

	// Zip contents match expectation.
	var zipKey string
	for _, k := range keys {
		if filepath.Ext(k) == ".zip" {
			zipKey = k
			break
		}
	}
	data, _ := store.GetBytes(zipKey)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	zipEntries := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		zipEntries[f.Name] = string(b)
	}
	want := map[string]string{"photos/a.jpg": "alpha", "photos/b.jpg": "bravo", "photos/c.jpg": "charlie"}
	for k, v := range want {
		if zipEntries[k] != v {
			t.Errorf("zip entry %s=%q want %q", k, zipEntries[k], v)
		}
	}

	// Sidecar index lists every entry in the zip, newline-separated, and
	// was uploaded to STANDARD so it can be read without a Glacier restore.
	indexKey := zipKey + ".index.txt"
	indexBytes, ok := store.GetBytes(indexKey)
	if !ok {
		t.Fatalf("missing zip index sidecar at %s", indexKey)
	}
	gotEntries := map[string]struct{}{}
	for _, line := range strings.Split(strings.TrimRight(string(indexBytes), "\n"), "\n") {
		gotEntries[line] = struct{}{}
	}
	for k := range want {
		if _, present := gotEntries[k]; !present {
			t.Errorf("zip index missing entry %q (body=%q)", k, string(indexBytes))
		}
	}
	head, err := store.Head(ctx, indexKey)
	if err != nil {
		t.Fatalf("head index: %v", err)
	}
	if head.StorageClass != "STANDARD" {
		t.Errorf("index storage class=%q want STANDARD", head.StorageClass)
	}
}

func TestEngineNoPendingFiles(t *testing.T) {
	eng, d, _, _, _, col := newTestEngine(t, 3)
	runID, err := eng.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, _ := d.GetRun(context.Background(), runID)
	if run.Status != db.RunCompleted {
		t.Errorf("status=%q want completed", run.Status)
	}
	if run.FilesUploaded != 0 {
		t.Errorf("uploaded=%d want 0", run.FilesUploaded)
	}
	if len(col.byType(EventUploadStart)) != 0 {
		t.Errorf("unexpected upload events: %+v", col.events)
	}
}

type failingStorage struct {
	storage.Storage
	putErr error
	calls  int
}

func (f *failingStorage) Put(ctx context.Context, key string, body io.Reader, size int64) (storage.PutResult, error) {
	f.calls++
	_, _ = io.Copy(io.Discard, body)
	if f.putErr != nil {
		return storage.PutResult{}, f.putErr
	}
	return storage.PutResult{Key: key, ETag: "\"x\"", Size: size}, nil
}

func TestEngineUploadFailure(t *testing.T) {
	eng, d, _, _, root, col := newTestEngine(t, 10) // high threshold -> individual
	writeFile(t, root, "a.txt", "hi")

	failing := &failingStorage{Storage: storage.NewMemStorage(), putErr: errors.New("boom")}
	eng.opts.Storage = failing

	ctx := context.Background()
	runID, err := eng.Run(ctx)
	if err == nil {
		t.Fatal("want run error")
	}
	run, _ := d.GetRun(ctx, runID)
	if run.Status != db.RunFailed {
		t.Errorf("status=%q want failed", run.Status)
	}
	if run.ErrorMessage == "" {
		t.Error("error_message not set")
	}

	// File should be marked failed.
	files, _, _ := d.ListFiles(ctx, db.FilesFilter{})
	if len(files) != 1 || files[0].Status != db.StatusFailed {
		t.Errorf("want 1 failed file, got %+v", files)
	}

	if len(col.byType(EventUploadFailed)) != 1 {
		t.Error("want 1 upload_failed event")
	}
}

// cancellingSource wraps a Source and calls cancel() inside the first
// Walk callback — lets us exercise "scan started, then cancelled" without
// the CreateRun call tripping on a pre-cancelled context.
type cancellingSource struct {
	inner  source.Source
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancellingSource) Walk(ctx context.Context, fn source.WalkFunc) error {
	return c.inner.Walk(ctx, func(e source.Entry) error {
		c.once.Do(c.cancel)
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(e)
	})
}
func (c *cancellingSource) Open(ctx context.Context, rel string) (io.ReadCloser, error) {
	return c.inner.Open(ctx, rel)
}
func (c *cancellingSource) Close() error { return c.inner.Close() }

func TestEngineCancellation(t *testing.T) {
	eng, d, innerSrc, _, root, _ := newTestEngine(t, 100)
	for i := 0; i < 20; i++ {
		writeFile(t, root, fmt.Sprintf("f%02d.txt", i), "x")
	}

	ctx, cancel := context.WithCancel(context.Background())
	eng.opts.Source = &cancellingSource{inner: innerSrc, cancel: cancel}

	runID, err := eng.Run(ctx)
	if err == nil {
		t.Fatal("want cancel error")
	}
	run, _ := d.GetRun(context.Background(), runID)
	if run.Status != db.RunCancelled {
		t.Errorf("status=%q want cancelled", run.Status)
	}
}

func TestEngineUsesClock(t *testing.T) {
	eng, d, _, _, root, _ := newTestEngine(t, 10)
	writeFile(t, root, "a.txt", "hi")

	fixed := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	eng.opts.Now = func() time.Time { return fixed }

	runID, err := eng.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run, _ := d.GetRun(context.Background(), runID)
	if !run.StartedAt.Equal(fixed) {
		t.Errorf("StartedAt=%v want %v", run.StartedAt, fixed)
	}
}
