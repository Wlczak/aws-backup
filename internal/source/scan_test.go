package source

import (
	"context"
	"os"
	"path/filepath"
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
	s, err := Scan(ctx, src, d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Seen != 2 || s.New != 2 || s.Changed != 0 || s.Missing != 0 {
		t.Errorf("first scan: %+v", s)
	}

	// Mark both as uploaded so we can see MarkMissing effect.
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

	s, err = Scan(ctx, src, d, nil, nil)
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
	if s.Missing != 1 {
		t.Errorf("missing=%d want 1", s.Missing)
	}

	byPath := map[string]string{}
	files, _, _ = d.ListFiles(ctx, db.FilesFilter{})
	for _, f := range files {
		byPath[f.Path] = f.Status
	}
	if byPath["a.txt"] != db.StatusPending {
		t.Errorf("a.txt status=%q want pending", byPath["a.txt"])
	}
	if byPath["keep.txt"] != db.StatusMissing {
		t.Errorf("keep.txt status=%q want missing", byPath["keep.txt"])
	}
	if byPath["c.txt"] != db.StatusPending {
		t.Errorf("c.txt status=%q want pending", byPath["c.txt"])
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
	if _, err := Scan(ctx, src, d, nil, nil); err == nil {
		t.Fatal("expected cancel error")
	}
}
