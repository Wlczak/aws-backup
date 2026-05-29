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
	"sync/atomic"
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
		ScanChunkSize:  2,
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

	// upload_plan must arrive once with the full file count so the UI
	// progress bar's denominator is correct from the first byte. (#126)
	plans := col.byType(EventUploadPlan)
	if len(plans) != 1 {
		t.Fatalf("want exactly 1 upload_plan event, got %d", len(plans))
	}
	if got := plans[0].Data["total_files"]; got != 6 {
		t.Errorf("upload_plan.total_files = %v, want 6", got)
	}
	if got := plans[0].Data["total_groups"]; got != 3 {
		t.Errorf("upload_plan.total_groups = %v, want 3", got)
	}

	// Each of the 4 upload keys (1 zip + 3 individual) must surface at
	// least one copy_progress event so the UI can show source→tmp
	// progress for slow reads. The final belt-and-braces 100% sample
	// emitted by the engine guarantees this even for tiny test files.
	// (#127)
	copies := col.byType(EventCopyProgress)
	gotKeys := map[string]bool{}
	for _, ev := range copies {
		if k, ok := ev.Data["key"].(string); ok {
			gotKeys[k] = true
		}
	}
	if len(gotKeys) != 4 {
		t.Errorf("copy_progress saw %d distinct keys, want 4 (one zip + 3 individual)", len(gotKeys))
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

// TestEngineGracefulStop ensures StopRequested fired between groups
// terminates the run cleanly with RunStopped status: the in-flight
// upload completes (no torn files), no further uploads start, and the
// run carries no error message. (#124)
func TestEngineGracefulStop(t *testing.T) {
	eng, d, _, store, root, col := newTestEngine(t, 100) // high zip threshold → all individual

	// Five separate top-dirs so each becomes its own group, giving the
	// engine multiple between-group check points where StopRequested
	// can flip to true.
	for _, dir := range []string{"a", "b", "c", "d", "e"} {
		writeFile(t, root, dir+"/file.txt", "data-"+dir)
	}

	// atomic: StopRequested polls from the copy-worker goroutine while
	// Emit fires from the upload-worker goroutine in the new pipeline.
	var stop atomic.Bool
	eng.opts.StopRequested = stop.Load
	// Flip after the first upload completes so we know mid-run stop works
	// (rather than refusing to start anything).
	origEmit := eng.opts.Emit
	eng.opts.Emit = func(ev Event) {
		origEmit(ev)
		if ev.Type == EventUploadComplete {
			stop.Store(true)
		}
	}

	ctx := context.Background()
	runID, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, _ := d.GetRun(ctx, runID)
	if run.Status != db.RunStopped {
		t.Errorf("status=%q want %q", run.Status, db.RunStopped)
	}
	if run.ErrorMessage != "" {
		t.Errorf("ErrorMessage=%q want empty", run.ErrorMessage)
	}
	completes := col.byType(EventUploadComplete)
	if len(completes) == 0 {
		t.Fatal("expected at least one upload to complete before stop")
	}
	if len(completes) >= 5 {
		t.Errorf("got %d completes, expected stop to short-circuit before all 5", len(completes))
	}
	// Storage should hold exactly the keys we observed completing,
	// proving no torn / partial uploads landed.
	if got := len(store.Keys()); got != len(completes) {
		t.Errorf("store has %d keys but %d uploads completed", got, len(completes))
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
	putErr   error
	failKeys map[string]bool // when set, only listed keys fail; others delegate to inner
	calls    int
}

func (f *failingStorage) Put(ctx context.Context, key string, body io.Reader, size int64) (storage.PutResult, error) {
	f.calls++
	if f.putErr != nil && (f.failKeys == nil || f.failKeys[key]) {
		_, _ = io.Copy(io.Discard, body)
		return storage.PutResult{}, f.putErr
	}
	if f.Storage != nil {
		return f.Storage.Put(ctx, key, body, size)
	}
	_, _ = io.Copy(io.Discard, body)
	return storage.PutResult{Key: key, ETag: "\"x\"", Size: size}, nil
}

func TestEngineUploadFailure(t *testing.T) {
	eng, d, _, _, root, col := newTestEngine(t, 10) // high threshold -> individual
	writeFile(t, root, "a.txt", "hi")
	writeFile(t, root, "b.txt", "ok")

	// Fail only on a.txt; b.txt should still upload successfully so we can
	// verify a per-file failure doesn't abort the rest of the run.
	mem := storage.NewMemStorage()
	failing := &failingStorage{Storage: mem, putErr: errors.New("boom"), failKeys: map[string]bool{"backups/a.txt": true}}
	eng.opts.Storage = failing

	ctx := context.Background()
	runID, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("run should succeed despite per-file failure, got %v", err)
	}
	run, _ := d.GetRun(ctx, runID)
	if run.Status != db.RunCompleted {
		t.Errorf("status=%q want completed", run.Status)
	}

	files, _, _ := d.ListFiles(ctx, db.FilesFilter{})
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = f.Status
	}
	if got["a.txt"] != db.StatusFailed {
		t.Errorf("a.txt status=%q want failed", got["a.txt"])
	}
	if got["b.txt"] != db.StatusUploaded {
		t.Errorf("b.txt status=%q want uploaded", got["b.txt"])
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

// TestZipCounterContinuesSequence verifies that a second run adding new files
// to a previously-zipped directory creates photos_2.zip rather than
// overwriting photos_1.zip.
func TestZipCounterContinuesSequence(t *testing.T) {
	eng, d, _, store, root, _ := newTestEngine(t, 3)
	ctx := context.Background()

	// First run: 3 files in photos/ → photos_1.zip.
	writeFile(t, root, "photos/a.jpg", "alpha")
	writeFile(t, root, "photos/b.jpg", "bravo")
	writeFile(t, root, "photos/c.jpg", "charlie")
	if _, err := eng.Run(ctx); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// Confirm photos_1.zip exists.
	keysAfterRun1 := store.Keys()
	var zip1Key string
	for _, k := range keysAfterRun1 {
		if strings.HasSuffix(k, "_1.zip") {
			zip1Key = k
		}
	}
	if zip1Key == "" {
		t.Fatalf("expected a _1.zip after run 1, keys=%v", keysAfterRun1)
	}

	// Second run: 3 more files in photos/ → should produce photos_2.zip.
	writeFile(t, root, "photos/d.jpg", "delta")
	writeFile(t, root, "photos/e.jpg", "echo")
	writeFile(t, root, "photos/f.jpg", "foxtrot")
	if _, err := eng.Run(ctx); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	// photos_1.zip must still exist (not overwritten).
	if _, ok := store.GetBytes(zip1Key); !ok {
		t.Errorf("photos_1.zip was overwritten: key %q missing after run 2", zip1Key)
	}

	// A new _2.zip must have been created.
	var zip2Key string
	for _, k := range store.Keys() {
		if strings.HasSuffix(k, "_2.zip") {
			zip2Key = k
		}
	}
	if zip2Key == "" {
		t.Errorf("expected a _2.zip after run 2, keys=%v", store.Keys())
	}

	// All 6 files should be uploaded.
	files, _, _ := d.ListFiles(ctx, db.FilesFilter{})
	for _, f := range files {
		if f.Status != db.StatusUploaded {
			t.Errorf("%s: status=%q want uploaded", f.Path, f.Status)
		}
	}
}

// TestEngineReconcileFromS3 simulates the crash window between a successful
// S3 put and the DB commit: the zip and its index exist in S3 but the DB rows
// are still pending. The next run must detect this via the index sidecars and
// mark the files uploaded without creating a duplicate zip.
func TestEngineReconcileFromS3(t *testing.T) {
	eng, d, _, store, root, _ := newTestEngine(t, 3)
	ctx := context.Background()

	writeFile(t, root, "photos/a.jpg", "alpha")
	writeFile(t, root, "photos/b.jpg", "bravo")
	writeFile(t, root, "photos/c.jpg", "charlie")

	// First run: uploads photos zip + index to S3 and marks DB uploaded.
	if _, err := eng.Run(ctx); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	keysAfterRun1 := store.Keys()

	// Simulate the crash: reset all files back to pending (as if the DB
	// commit after the successful S3 put never happened).
	allFiles, _, _ := d.ListFiles(ctx, db.FilesFilter{All: true})
	paths := make([]string, len(allFiles))
	for i, f := range allFiles {
		paths[i] = f.Path
	}
	if _, err := d.MarkPendingByPaths(ctx, paths); err != nil {
		t.Fatalf("reset to pending: %v", err)
	}

	// Second run: reconcile should detect the existing zip+index in S3,
	// mark files uploaded, and produce no new S3 objects.
	if _, err := eng.Run(ctx); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	keysAfterRun2 := store.Keys()
	if len(keysAfterRun2) != len(keysAfterRun1) {
		t.Errorf("S3 object count changed during reconcile run: before=%d after=%d\nbefore=%v\nafter=%v",
			len(keysAfterRun1), len(keysAfterRun2), keysAfterRun1, keysAfterRun2)
	}

	// All files must be uploaded with zip_name set.
	filesAfter, _, _ := d.ListFiles(ctx, db.FilesFilter{All: true})
	for _, f := range filesAfter {
		if f.Status != db.StatusUploaded {
			t.Errorf("%s: status=%q want uploaded", f.Path, f.Status)
		}
		if f.ZipName == "" {
			t.Errorf("%s: zip_name empty after reconcile", f.Path)
		}
	}
}

// TestEngineSkipsIndividualWhenSHA256Matches: when a pending individual
// file is re-run and S3 already holds an object at the same key with a
// matching SHA256, the engine must skip the upload, mark the row
// uploaded, and emit upload_complete with skipped=true (no
// upload_start / no upload_progress for that key). (#133)
func TestEngineSkipsIndividualWhenSHA256Matches(t *testing.T) {
	eng, d, _, store, root, col := newTestEngine(t, 100) // threshold high → individual path
	ctx := context.Background()
	writeFile(t, root, "alpha.txt", "alpha-content")

	if _, err := eng.Run(ctx); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// Reset the row to pending so the next run hits the upload path again.
	files, _, _ := d.ListFiles(ctx, db.FilesFilter{})
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	if _, err := d.MarkPendingByIDs(ctx, []int64{files[0].ID}); err != nil {
		t.Fatal(err)
	}

	// Capture the existing object's bytes — they must not change after the
	// skipped run.
	const key = "backups/alpha.txt"
	bytesBefore, ok := store.GetBytes(key)
	if !ok {
		t.Fatalf("missing object at %s after first run", key)
	}

	col.events = nil
	if _, err := eng.Run(ctx); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	bytesAfter, _ := store.GetBytes(key)
	if !bytes.Equal(bytesBefore, bytesAfter) {
		t.Errorf("object bytes changed across skipped run")
	}

	// No upload_start for the dedup'd key; upload_complete must carry skipped=true.
	for _, ev := range col.byType(EventUploadStart) {
		if ev.Data["key"] == key {
			t.Errorf("unexpected upload_start for %s on skipped run", key)
		}
	}
	var sawSkipped bool
	for _, ev := range col.byType(EventUploadComplete) {
		if ev.Data["key"] == key && ev.Data["skipped"] == true {
			sawSkipped = true
		}
	}
	if !sawSkipped {
		t.Errorf("expected upload_complete with skipped=true for %s", key)
	}

	// DB row marked uploaded.
	files2, _, _ := d.ListFiles(ctx, db.FilesFilter{})
	if files2[0].Status != db.StatusUploaded {
		t.Errorf("row status=%q want uploaded", files2[0].Status)
	}
}

// TestEngineUploadsWhenSHA256Differs: a re-run of a pending file whose
// destination key already holds a *different* object must still upload —
// dedup is byte-identical-only. (#133)
func TestEngineUploadsWhenSHA256Differs(t *testing.T) {
	eng, d, _, store, root, col := newTestEngine(t, 100)
	ctx := context.Background()
	writeFile(t, root, "beta.txt", "new-content")

	const key = "backups/beta.txt"
	// Pre-seed with different bytes so SHA256 won't match.
	if _, err := store.Put(ctx, key, bytes.NewReader([]byte("STALE-PRESEED")), -1); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.Run(ctx); err != nil {
		t.Fatal(err)
	}

	got, _ := store.GetBytes(key)
	if string(got) != "new-content" {
		t.Errorf("object not replaced: got %q want %q", string(got), "new-content")
	}
	for _, ev := range col.byType(EventUploadComplete) {
		if ev.Data["key"] == key && ev.Data["skipped"] == true {
			t.Errorf("upload_complete should not be skipped when SHA256 differs")
		}
	}
	files, _, _ := d.ListFiles(ctx, db.FilesFilter{})
	if files[0].Status != db.StatusUploaded {
		t.Errorf("row status=%q want uploaded", files[0].Status)
	}
}

// TestSkipIfMatches exercises the dedup primitive directly: it must
// return true only when S3 already holds an object whose SHA256
// matches the supplied hex, and false in every other case (key
// missing, checksum unknown, content differs, empty input). Call
// sites at engine.go's three Put points wrap this verbatim. (#133)
func TestSkipIfMatches(t *testing.T) {
	eng, _, _, store, _, _ := newTestEngine(t, 100)
	ctx := context.Background()

	const key = "backups/probe.bin"
	body := []byte("identical-content")
	res, err := store.Put(ctx, key, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	matchHex := res.ChecksumSHA256
	if matchHex == "" {
		t.Fatal("MemStorage.Put didn't echo a SHA256 — fixture broken")
	}

	if !eng.skipIfMatches(ctx, key, matchHex) {
		t.Errorf("expected true for matching SHA256 at existing key")
	}
	if eng.skipIfMatches(ctx, key, "0000000000000000000000000000000000000000000000000000000000000000") {
		t.Errorf("expected false when SHA256 differs from existing object")
	}
	if eng.skipIfMatches(ctx, "backups/missing.bin", matchHex) {
		t.Errorf("expected false for non-existent key")
	}
	if eng.skipIfMatches(ctx, key, "") {
		t.Errorf("expected false for empty local SHA256")
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

// indTmpPath finds the engine's stable per-file tmp path for the row
// matching rel. Encapsulates the naming convention so tests don't have
// to hard-code it. (#127)
func indTmpPath(t *testing.T, d *db.DB, tmpDir, rel string) string {
	t.Helper()
	files, _, _ := d.ListFiles(context.Background(), db.FilesFilter{})
	for _, f := range files {
		if f.Path == rel {
			return filepath.Join(tmpDir, fmt.Sprintf("ind-%d", f.ID))
		}
	}
	t.Fatalf("no DB row for %s", rel)
	return ""
}

// TestEngineKeepsTmpOnUploadFailure: when the source→tmp copy succeeds
// but the S3 upload fails, the tmp file must survive so the next run
// can re-upload from it without re-reading the (potentially slow)
// source. (#127)
func TestEngineKeepsTmpOnUploadFailure(t *testing.T) {
	eng, d, _, _, root, _ := newTestEngine(t, 100) // high threshold → individual
	writeFile(t, root, "big.bin", "the-payload")

	mem := storage.NewMemStorage()
	failing := &failingStorage{Storage: mem, putErr: errors.New("boom"), failKeys: map[string]bool{"backups/big.bin": true}}
	eng.opts.Storage = failing

	ctx := context.Background()
	if _, err := eng.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	tmp := indTmpPath(t, d, eng.opts.TmpDir, "big.bin")
	info, err := os.Stat(tmp)
	if err != nil {
		t.Fatalf("tmp %s should survive upload failure, got: %v", tmp, err)
	}
	if info.Size() != int64(len("the-payload")) {
		t.Errorf("tmp size=%d want %d", info.Size(), len("the-payload"))
	}
}

// TestEngineReusesTmpOnSecondRun: after a run-1 upload failure leaves a
// cached tmp, run 2 must skip copyAndHash entirely (no copy_progress
// events for that key) and still mark the row uploaded. The wire signal
// that copyAndHash didn't run is the absence of copy_progress for that
// key — copyAndHash is the sole emitter for individual files. (#127)
func TestEngineReusesTmpOnSecondRun(t *testing.T) {
	eng, d, _, _, root, col := newTestEngine(t, 100)
	eng.opts.RetryFailed = true // mirror config default so the failed row re-queues
	writeFile(t, root, "big.bin", "the-payload")

	mem := storage.NewMemStorage()
	failing := &failingStorage{Storage: mem, putErr: errors.New("boom"), failKeys: map[string]bool{"backups/big.bin": true}}
	eng.opts.Storage = failing

	ctx := context.Background()
	if _, err := eng.Run(ctx); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// Run 2 with normal storage → upload should succeed and reuse tmp.
	eng.opts.Storage = mem
	col.events = nil
	if _, err := eng.Run(ctx); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	// File ends uploaded.
	files, _, _ := d.ListFiles(ctx, db.FilesFilter{})
	if files[0].Status != db.StatusUploaded {
		t.Errorf("status=%q want uploaded", files[0].Status)
	}

	// No copy_progress event for our key → tryReuseTmp short-circuited.
	for _, ev := range col.byType(EventCopyProgress) {
		if k, _ := ev.Data["key"].(string); k == "backups/big.bin" {
			t.Errorf("unexpected copy_progress for backups/big.bin on reused-tmp run: %+v", ev.Data)
		}
	}

	// And the upload still happened (object exists in mem).
	if _, ok := mem.GetBytes("backups/big.bin"); !ok {
		t.Errorf("expected object backups/big.bin to be uploaded on run 2")
	}
}

// TestEngineRecopiesWhenSourceChangedAfterTmp: if the source file is
// modified between the failed run-1 and run 2, source.Scan advances the
// DB row's mtime past the cached tmp's mtime; tryReuseTmp must reject
// the cached tmp and let copyAndHash overwrite it with the new
// content. (#127)
func TestEngineRecopiesWhenSourceChangedAfterTmp(t *testing.T) {
	eng, d, _, _, root, _ := newTestEngine(t, 100)
	eng.opts.RetryFailed = true
	writeFile(t, root, "big.bin", "old-content")

	mem := storage.NewMemStorage()
	failing := &failingStorage{Storage: mem, putErr: errors.New("boom"), failKeys: map[string]bool{"backups/big.bin": true}}
	eng.opts.Storage = failing

	ctx := context.Background()
	if _, err := eng.Run(ctx); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// Modify source: new content + advanced mtime.
	writeFile(t, root, "big.bin", "new-content-different-length")
	newMTime := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "big.bin"), newMTime, newMTime); err != nil {
		t.Fatal(err)
	}

	// Run 2 with normal storage — must upload the NEW content.
	eng.opts.Storage = mem
	if _, err := eng.Run(ctx); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	got, ok := mem.GetBytes("backups/big.bin")
	if !ok {
		t.Fatal("object missing after run 2")
	}
	if string(got) != "new-content-different-length" {
		t.Errorf("uploaded bytes = %q, want new content; tryReuseTmp must have rejected the stale tmp", string(got))
	}

	files, _, _ := d.ListFiles(ctx, db.FilesFilter{})
	if files[0].Status != db.StatusUploaded {
		t.Errorf("status=%q want uploaded", files[0].Status)
	}
}

// TestEngineSweepsOrphanTmps: an `ind-{N}` file in tmp whose ID is not
// in the current pending list must be removed by the upload-phase
// orphan sweep. Without this, every leave-on-failure tmp would leak
// across many runs. (#127)
func TestEngineSweepsOrphanTmps(t *testing.T) {
	eng, _, _, _, root, _ := newTestEngine(t, 100)
	writeFile(t, root, "real.txt", "x") // gives the run something to do so it reaches the upload phase

	// Pre-seed an orphan tmp with a fileID that won't exist in the DB.
	orphan := filepath.Join(eng.opts.TmpDir, "ind-999999")
	if err := os.WriteFile(orphan, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan tmp %s should have been swept; stat err=%v", orphan, err)
	}
}
