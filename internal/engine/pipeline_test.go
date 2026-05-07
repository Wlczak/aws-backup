package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/source"
	"github.com/Wlczak/aws-backup/internal/storage"
)

// newPipelineEngine is like newTestEngine but accepts explicit thread counts.
func newPipelineEngine(t *testing.T, ct, ut, pq, thresh int) (*Engine, *db.DB, *source.LocalDir, *storage.MemStorage, string, *collector) {
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
		ChunkSize:      10,
		ZipThresh:      thresh,
		EnableZipIndex: true,
		CopyThreads:    ct,
		UploadThreads:  ut,
		PipelineQueue:  pq,
		Emit:           col.emit,
	})
	return eng, d, src, store, root, col
}

// TestPipeline_SequentialEquivalence verifies that 1/1 threads produces the
// same result (all files uploaded, all DB rows marked uploaded).
func TestPipeline_SequentialEquivalence(t *testing.T) {
	eng, d, _, store, root, _ := newPipelineEngine(t, 1, 1, 0, 50)
	ctx := context.Background()

	for i := range 5 {
		writeFile(t, root, fmt.Sprintf("file%d.txt", i), fmt.Sprintf("content%d", i))
	}
	_, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, _ := d.GetRun(ctx, 1)
	if run.Status != db.RunCompleted {
		t.Fatalf("status=%q want completed", run.Status)
	}
	if run.FilesUploaded != 5 {
		t.Errorf("uploaded=%d want 5", run.FilesUploaded)
	}
	if got := len(store.Keys()); got != 5 {
		t.Errorf("stored keys=%d want 5", got)
	}
}

// TestPipeline_ConcurrentTwoThreads verifies that copy=2/upload=2 uploads all
// files correctly (no double-upload, no missing file).
func TestPipeline_ConcurrentTwoThreads(t *testing.T) {
	eng, d, _, store, root, _ := newPipelineEngine(t, 2, 2, 2, 50)
	ctx := context.Background()

	for i := range 10 {
		writeFile(t, root, fmt.Sprintf("file%02d.txt", i), fmt.Sprintf("data%d", i))
	}
	_, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, _ := d.GetRun(ctx, 1)
	if run.Status != db.RunCompleted {
		t.Fatalf("status=%q want completed", run.Status)
	}
	if run.FilesUploaded != 10 {
		t.Errorf("uploaded=%d want 10", run.FilesUploaded)
	}
	if got := len(store.Keys()); got != 10 {
		t.Errorf("stored keys=%d want 10", got)
	}
}

// TestPipeline_ZipKeyCollisionRetry verifies that an ErrAlreadyExists from
// PutIfAbsent causes the group to be re-staged with a new slot and eventually
// succeeds.
func TestPipeline_ZipKeyCollisionRetry(t *testing.T) {
	root := t.TempDir()
	tmp := t.TempDir()
	src, err := source.NewLocalDir(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "engine.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); src.Close() })

	var putCount atomic.Int32
	store := &collidingStorage{
		MemStorage:  storage.NewMemStorage(),
		collideUntil: 1,
		count:        &putCount,
	}

	col := &collector{}
	eng := New(Options{
		DB:             d,
		Source:         src,
		Storage:        store,
		TmpDir:         tmp,
		KeyPrefix:      "backups",
		ChunkSize:      10,
		ZipThresh:      2,
		EnableZipIndex: true,
		CopyThreads:    1,
		UploadThreads:  1,
		PipelineQueue:  1,
		Emit:           col.emit,
	})

	ctx := context.Background()
	// Two files in same dir so they zip (threshold=2).
	writeFile(t, root, "dir/a.txt", "aaa")
	writeFile(t, root, "dir/b.txt", "bbb")

	_, runErr := eng.Run(ctx)
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}

	run, _ := d.GetRun(ctx, 1)
	if run.Status != db.RunCompleted {
		t.Fatalf("status=%q want completed", run.Status)
	}
	if run.FilesUploaded != 2 {
		t.Errorf("uploaded=%d want 2", run.FilesUploaded)
	}
	// PutIfAbsent called at least twice (1 collision + 1 success).
	if n := putCount.Load(); n < 2 {
		t.Errorf("PutIfAbsent called %d times, want >=2", n)
	}
}

// collidingStorage wraps MemStorage and returns ErrAlreadyExists for the
// first `collideUntil` PutIfAbsent calls.
type collidingStorage struct {
	*storage.MemStorage
	collideUntil int32
	count        *atomic.Int32
}

func (c *collidingStorage) PutIfAbsent(ctx context.Context, key string, body io.Reader, size int64) (storage.PutResult, error) {
	n := c.count.Add(1)
	if n <= c.collideUntil {
		io.Copy(io.Discard, body)
		return storage.PutResult{}, storage.ErrAlreadyExists
	}
	return c.MemStorage.PutIfAbsent(ctx, key, body, size)
}

// TestPipeline_ZipKeyCollisionExhausted verifies that after exhausting all
// retries (>4 collisions) the group is counted as an error.
func TestPipeline_ZipKeyCollisionExhausted(t *testing.T) {
	root := t.TempDir()
	tmp := t.TempDir()
	src, err := source.NewLocalDir(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "engine.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); src.Close() })

	var count atomic.Int32
	store := &collidingStorage{
		MemStorage:  storage.NewMemStorage(),
		collideUntil: 100, // never succeeds
		count:        &count,
	}

	col := &collector{}
	eng := New(Options{
		DB:             d,
		Source:         src,
		Storage:        store,
		TmpDir:         tmp,
		KeyPrefix:      "backups",
		ChunkSize:      10,
		ZipThresh:      2,
		EnableZipIndex: true,
		CopyThreads:    1,
		UploadThreads:  1,
		PipelineQueue:  1,
		Emit:           col.emit,
	})

	ctx := context.Background()
	writeFile(t, root, "dir/a.txt", "aaa")
	writeFile(t, root, "dir/b.txt", "bbb")

	_, runErr := eng.Run(ctx)
	if runErr == nil {
		t.Error("expected run error for exhausted retries, got nil")
	}

	run, _ := d.GetRun(ctx, 1)
	if run.Status != db.RunFailed {
		t.Errorf("status=%q want failed", run.Status)
	}
}

// TestPipeline_StopRequestedDrains verifies that when StopRequested fires,
// the run terminates as RunStopped without error.
func TestPipeline_StopRequestedDrains(t *testing.T) {
	root := t.TempDir()
	tmp := t.TempDir()
	src, err := source.NewLocalDir(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "engine.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); src.Close() })

	store := storage.NewMemStorage()
	col := &collector{}

	stopCh := make(chan struct{})
	var stopOnce atomic.Bool

	eng := New(Options{
		DB:             d,
		Source:         src,
		Storage:        store,
		TmpDir:         tmp,
		KeyPrefix:      "backups",
		ChunkSize:      10,
		ZipThresh:      50,
		EnableZipIndex: true,
		CopyThreads:    2,
		UploadThreads:  2,
		PipelineQueue:  2,
		Emit:           col.emit,
		StopRequested: func() bool {
			select {
			case <-stopCh:
				return true
			default:
				return false
			}
		},
	})

	ctx := context.Background()
	for i := range 20 {
		writeFile(t, root, fmt.Sprintf("file%02d.txt", i), fmt.Sprintf("content%d", i))
	}

	go func() {
		time.Sleep(5 * time.Millisecond)
		if stopOnce.CompareAndSwap(false, true) {
			close(stopCh)
		}
	}()

	_, runErr := eng.Run(ctx)
	if runErr != nil {
		t.Fatalf("Run returned unexpected error: %v", runErr)
	}

	run, _ := d.GetRun(ctx, 1)
	if run.Status != db.RunStopped && run.Status != db.RunCompleted {
		t.Errorf("status=%q want stopped or completed", run.Status)
	}
}

// TestPipeline_ContextCancel verifies context cancellation terminates cleanly.
func TestPipeline_ContextCancel(t *testing.T) {
	root := t.TempDir()
	tmp := t.TempDir()
	src, err := source.NewLocalDir(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "engine.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); src.Close() })

	store := storage.NewMemStorage()
	col := &collector{}

	eng := New(Options{
		DB:             d,
		Source:         src,
		Storage:        store,
		TmpDir:         tmp,
		KeyPrefix:      "backups",
		ChunkSize:      10,
		ZipThresh:      50,
		EnableZipIndex: true,
		CopyThreads:    2,
		UploadThreads:  2,
		PipelineQueue:  2,
		Emit:           col.emit,
	})

	ctx := context.Background()
	for i := range 30 {
		writeFile(t, root, fmt.Sprintf("file%02d.txt", i), fmt.Sprintf("data%d", i))
	}

	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	runID, runErr := eng.Run(runCtx)
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		t.Errorf("unexpected error: %v", runErr)
	}
	// Cancel that fires before CreateRun returns: no run row was ever
	// written, so there's nothing to assert about its status. The
	// cancel-error check above is enough.
	if runID == 0 {
		return
	}
	run, _ := d.GetRun(ctx, runID)
	if run.Status != db.RunCancelled && run.Status != db.RunCompleted {
		t.Errorf("status=%q want cancelled or completed", run.Status)
	}
}

// TestPipeline_PerGroupErrorIsolation verifies that one group failing does not
// abort the remaining groups.
func TestPipeline_PerGroupErrorIsolation(t *testing.T) {
	root := t.TempDir()
	tmp := t.TempDir()
	src, err := source.NewLocalDir(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "engine.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); src.Close() })

	store := &partialFailStorage{MemStorage: storage.NewMemStorage(), failPrefix: "backups/bad/"}
	col := &collector{}
	eng := New(Options{
		DB:             d,
		Source:         src,
		Storage:        store,
		TmpDir:         tmp,
		KeyPrefix:      "backups",
		ChunkSize:      10,
		ZipThresh:      50,
		EnableZipIndex: true,
		CopyThreads:    2,
		UploadThreads:  2,
		PipelineQueue:  2,
		Emit:           col.emit,
	})

	ctx := context.Background()
	// 5 good files in one dir, 1 bad file in another dir (separate groups).
	for i := range 5 {
		writeFile(t, root, fmt.Sprintf("good/file%d.txt", i), fmt.Sprintf("good%d", i))
	}
	writeFile(t, root, "bad/broken.txt", "broken")

	_, runErr := eng.Run(ctx)
	// Not all groups failed -> completed, not failed.
	if runErr != nil {
		t.Fatalf("Run returned unexpected error: %v", runErr)
	}

	run, _ := d.GetRun(ctx, 1)
	if run.Status != db.RunCompleted {
		t.Errorf("status=%q want completed", run.Status)
	}
	// 5 good files uploaded.
	if run.FilesUploaded != 5 {
		t.Errorf("uploaded=%d want 5", run.FilesUploaded)
	}
}

// partialFailStorage fails Put for keys with a given prefix.
type partialFailStorage struct {
	*storage.MemStorage
	failPrefix string
}

func (s *partialFailStorage) Put(ctx context.Context, key string, body io.Reader, size int64) (storage.PutResult, error) {
	if strings.HasPrefix(key, s.failPrefix) {
		io.Copy(io.Discard, body)
		return storage.PutResult{}, errors.New("simulated upload failure")
	}
	return s.MemStorage.Put(ctx, key, body, size)
}

// TestPipeline_AllGroupsFail verifies that when every group fails the run
// status is RunFailed. Uses a zip group (threshold=1) so that the storage
// failure is a hard group error rather than a per-file soft error.
func TestPipeline_AllGroupsFail(t *testing.T) {
	root := t.TempDir()
	tmp := t.TempDir()
	src, err := source.NewLocalDir(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "engine.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(); src.Close() })

	store := &alwaysFailStorage{}
	col := &collector{}
	eng := New(Options{
		DB:             d,
		Source:         src,
		Storage:        store,
		TmpDir:         tmp,
		KeyPrefix:      "backups",
		ChunkSize:      10,
		ZipThresh:      1, // threshold=1: single file becomes a zip group
		EnableZipIndex: true,
		CopyThreads:    1,
		UploadThreads:  1,
		PipelineQueue:  1,
		Emit:           col.emit,
	})

	ctx := context.Background()
	// Single file → one zip group; zip index upload fails → hard group error.
	writeFile(t, root, "file.txt", "hello")

	_, runErr := eng.Run(ctx)
	if runErr == nil {
		t.Error("expected run error, got nil")
	}

	run, _ := d.GetRun(ctx, 1)
	if run.Status != db.RunFailed {
		t.Errorf("status=%q want failed", run.Status)
	}
}

// alwaysFailStorage fails every storage operation.
type alwaysFailStorage struct{}

func (s *alwaysFailStorage) Put(_ context.Context, _ string, body io.Reader, _ int64) (storage.PutResult, error) {
	io.Copy(io.Discard, body)
	return storage.PutResult{}, errors.New("always fails")
}

func (s *alwaysFailStorage) PutStandard(_ context.Context, _ string, body io.Reader, _ int64) (storage.PutResult, error) {
	io.Copy(io.Discard, body)
	return storage.PutResult{}, errors.New("always fails")
}

func (s *alwaysFailStorage) PutIfAbsent(_ context.Context, _ string, body io.Reader, _ int64) (storage.PutResult, error) {
	io.Copy(io.Discard, body)
	return storage.PutResult{}, errors.New("always fails")
}

func (s *alwaysFailStorage) Head(_ context.Context, _ string) (storage.HeadResult, error) {
	return storage.HeadResult{}, errors.New("not found")
}

func (s *alwaysFailStorage) List(_ context.Context, _ string) ([]string, error) { return nil, nil }

func (s *alwaysFailStorage) Get(_ context.Context, _ string) (io.ReadCloser, error) {
	return nil, errors.New("not found")
}

func (s *alwaysFailStorage) Restore(_ context.Context, _ string, _ int) error { return nil }
func (s *alwaysFailStorage) Delete(_ context.Context, _ string) error          { return nil }
func (s *alwaysFailStorage) Close() error                                       { return nil }

// TestPipeline_ConfigOptionsDefaults verifies that 0/1 thread counts are
// normalized correctly by New().
func TestPipeline_ConfigOptionsDefaults(t *testing.T) {
	root := t.TempDir()
	src, _ := source.NewLocalDir(root)
	defer src.Close()
	d, _ := db.Open(context.Background(), filepath.Join(t.TempDir(), "engine.db"))
	defer d.Close()

	eng := New(Options{
		DB:            d,
		Source:        src,
		Storage:       storage.NewMemStorage(),
		TmpDir:        t.TempDir(),
		CopyThreads:   0, // should normalize to 1
		UploadThreads: 0, // should normalize to 1
		PipelineQueue: 0, // should normalize to max(ut,1)=1
	})

	if eng.opts.CopyThreads != 1 {
		t.Errorf("CopyThreads=%d want 1", eng.opts.CopyThreads)
	}
	if eng.opts.UploadThreads != 1 {
		t.Errorf("UploadThreads=%d want 1", eng.opts.UploadThreads)
	}
	if eng.opts.PipelineQueue != 1 {
		t.Errorf("PipelineQueue=%d want 1", eng.opts.PipelineQueue)
	}
}

// TestPipeline_HigherQueueDepth verifies that a pipeline queue > 1 works
// correctly (staged items accumulate then drain).
func TestPipeline_HigherQueueDepth(t *testing.T) {
	eng, d, _, store, root, _ := newPipelineEngine(t, 3, 1, 4, 50)
	ctx := context.Background()

	for i := range 8 {
		writeFile(t, root, fmt.Sprintf("f%d.txt", i), fmt.Sprintf("body%d", i))
	}
	_, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, _ := d.GetRun(ctx, 1)
	if run.Status != db.RunCompleted {
		t.Fatalf("status=%q want completed", run.Status)
	}
	if run.FilesUploaded != 8 {
		t.Errorf("uploaded=%d want 8", run.FilesUploaded)
	}
	if got := len(store.Keys()); got != 8 {
		t.Errorf("stored keys=%d want 8", got)
	}
}

// TestPipeline_ZipGroupConcurrent verifies zip groups are correctly handled
// with multiple copy threads (no slot collision, all files uploaded).
func TestPipeline_ZipGroupConcurrent(t *testing.T) {
	// ZipThresh=2: any dir with >=2 files gets zipped.
	eng, d, _, store, root, _ := newPipelineEngine(t, 2, 2, 2, 2)
	ctx := context.Background()

	// Three directories, each with 2 files -> 3 separate zip groups.
	for _, dir := range []string{"alpha", "beta", "gamma"} {
		writeFile(t, root, dir+"/a.dat", "aaaa")
		writeFile(t, root, dir+"/b.dat", "bbbb")
	}
	_, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, _ := d.GetRun(ctx, 1)
	if run.Status != db.RunCompleted {
		t.Fatalf("status=%q want completed", run.Status)
	}
	if run.FilesUploaded != 6 {
		t.Errorf("uploaded=%d want 6", run.FilesUploaded)
	}

	// Expect 3 zips + 3 index sidecars.
	keys := store.Keys()
	var zipCount, idxCount int
	for _, k := range keys {
		switch {
		case strings.HasSuffix(k, ".zip"):
			zipCount++
		case strings.HasSuffix(k, ".zip.index.txt"):
			idxCount++
		}
	}
	if zipCount != 3 {
		t.Errorf("zip count=%d want 3", zipCount)
	}
	if idxCount != 3 {
		t.Errorf("index count=%d want 3", idxCount)
	}
}
