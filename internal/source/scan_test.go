package source

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "scan.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestScanNewChangedMissing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "aa")
	writeFile(t, filepath.Join(root, "keep.txt"), "kept")

	src, _ := NewLocalDir(root)
	defer src.Close()
	d := openDB(t)

	// First scan: 2 new files.
	s, err := Scan(ctx, src, d, nil, nil, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.Seen != 2 || s.New != 2 || s.Changed != 0 || s.Missing != 0 {
		t.Errorf("first scan: %+v", s)
	}

	// Mark both as uploaded so we can see the disappearance reclassification.
	files, _, _ := d.ListFiles(ctx, db.FilesFilter{})
	for _, f := range files {
		if err := d.MarkUploaded(ctx, f.ID, "md5", "k/"+f.Path, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}

	// Change 'a.txt', remove 'keep.txt', add 'c.txt'.
	// Force a new mtime for a.txt.
	writeFile(t, filepath.Join(root, "a.txt"), "AAAA")
	newTime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(root, "a.txt"), newTime, newTime); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(root, "keep.txt"))
	writeFile(t, filepath.Join(root, "c.txt"), "c")

	// Sleep so scanStart is strictly after the last upload's last_seen_at.
	time.Sleep(10 * time.Millisecond)

	s, err = Scan(ctx, src, d, nil, nil, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.Seen != 2 {
		t.Errorf("seen=%d want 2", s.Seen)
	}
	if s.New != 1 {
		t.Errorf("new=%d want 1", s.New)
	}
	if s.Changed != 1 {
		t.Errorf("changed=%d want 1", s.Changed)
	}
	if s.Missing != 0 {
		t.Errorf("missing=%d want 0", s.Missing)
	}

	byPath := map[string]string{}
	files, _, _ = d.ListFiles(ctx, db.FilesFilter{})
	for _, f := range files {
		byPath[f.Path] = f.Status
	}
	if byPath["a.txt"] != db.StatusPending {
		t.Errorf("a.txt status=%q want pending", byPath["a.txt"])
	}
	if byPath["keep.txt"] != db.StatusCloudOnly {
		t.Errorf("keep.txt status=%q want cloud_only", byPath["keep.txt"])
	}
	if byPath["c.txt"] != db.StatusPending {
		t.Errorf("c.txt status=%q want pending", byPath["c.txt"])
	}
}

func TestScanRevivesMissingRowToPending(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "return.txt"), "back again")

	src, _ := NewLocalDir(root)
	defer src.Close()
	d := openDB(t)

	if _, err := Scan(ctx, src, d, nil, nil, nil, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkMissingByPaths(ctx, []string{"return.txt"}); err != nil {
		t.Fatalf("mark missing: %v", err)
	}

	s, err := Scan(ctx, src, d, nil, nil, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.Seen != 1 || s.Missing != 0 {
		t.Fatalf("rescan stats: %+v", s)
	}
	files, _, err := d.ListFiles(ctx, db.FilesFilter{Search: "return.txt", All: true})
	if err != nil {
		t.Fatalf("reload return.txt: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("return.txt rows=%d want 1", len(files))
	}
	if files[0].Status != db.StatusPending {
		t.Fatalf("return.txt status=%q want pending", files[0].Status)
	}
}

func TestScanProgressCallback(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const n = 7
	for i := 0; i < n; i++ {
		writeFile(t, filepath.Join(root, "f"+string(rune('0'+i))+".txt"), "x")
	}
	src, _ := NewLocalDir(root)
	defer src.Close()
	d := openDB(t)

	var samples []ScanProgress
	s, err := Scan(ctx, src, d, nil, nil, func(p ScanProgress) {
		samples = append(samples, p)
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.Seen != n {
		t.Fatalf("seen=%d want %d", s.Seen, n)
	}
	if len(samples) == 0 {
		t.Fatal("expected at least one progress callback")
	}
	last := samples[len(samples)-1]
	if last.Seen != n {
		t.Fatalf("final progress seen=%d want %d", last.Seen, n)
	}
	if last.New != n {
		t.Fatalf("final progress new=%d want %d", last.New, n)
	}
}

func TestScanContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		writeFile(t, filepath.Join(root, "f"+string(rune('0'+i))+".txt"), "x")
	}
	src, _ := NewLocalDir(root)
	defer src.Close()
	d := openDB(t)

	cancel() // cancel before scan runs
	if _, err := Scan(ctx, src, d, nil, nil, nil, 10); err == nil {
		t.Fatal("expected cancel error")
	}
}

type scanSpy struct {
	mu      sync.Mutex
	batches [][]string
}

func (s *scanSpy) UpsertFileBatch(ctx context.Context, entries []db.BatchEntry, seenAt time.Time) ([]db.UpsertResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := make([]string, len(entries))
	for i, e := range entries {
		batch[i] = e.Path
	}
	s.batches = append(s.batches, batch)
	results := make([]db.UpsertResult, len(entries))
	for i := range results {
		results[i].Created = true
	}
	return results, nil
}

func (s *scanSpy) MarkMissing(ctx context.Context, scanStart time.Time) (int64, error) {
	return 0, nil
}

func TestScanFlushesByBatchSize(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		writeFile(t, filepath.Join(root, "f"+string(rune('0'+i))+".txt"), "x")
	}
	src, _ := NewLocalDir(root)
	defer src.Close()
	spy := &scanSpy{}

	s, err := Scan(ctx, src, spy, nil, nil, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if s.Seen != 5 || s.New != 5 {
		t.Fatalf("scan stats: %+v", s)
	}

	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.batches) != 3 {
		t.Fatalf("batches=%d want 3", len(spy.batches))
	}
	if got := len(spy.batches[0]); got != 2 {
		t.Fatalf("first batch size=%d want 2", got)
	}
	if got := len(spy.batches[1]); got != 2 {
		t.Fatalf("second batch size=%d want 2", got)
	}
	if got := len(spy.batches[2]); got != 1 {
		t.Fatalf("third batch size=%d want 1", got)
	}
}
