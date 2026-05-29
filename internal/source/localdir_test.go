package source

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLocalDirWalk(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "top.txt"), "a")
	writeFile(t, filepath.Join(root, "sub", "a.txt"), "bb")
	writeFile(t, filepath.Join(root, "sub", "nested", "b.txt"), "ccc")
	// Non-regular: a directory is skipped implicitly.
	_ = os.MkdirAll(filepath.Join(root, "emptydir"), 0o755)

	src, err := NewLocalDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	var got []string
	err = src.Walk(context.Background(), func(e Entry) error {
		if e.IsDir {
			return nil
		}
		got = append(got, e.RelPath)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"sub/a.txt", "sub/nested/b.txt", "top.txt"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestLocalDirOpen(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "x", "hello.txt"), "hi there")

	src, _ := NewLocalDir(root)
	rc, err := src.Open(context.Background(), "x/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hi there" {
		t.Errorf("content=%q want %q", b, "hi there")
	}
}

func TestLocalDirOpenEscape(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "secret.txt"), "nope")

	src, _ := NewLocalDir(root)
	rel, err := filepath.Rel(root, filepath.Join(other, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Open(context.Background(), filepath.ToSlash(rel))
	if err == nil {
		t.Fatalf("expected escape error, got nil")
	}
}

func TestNewLocalDirMissing(t *testing.T) {
	if _, err := NewLocalDir(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing root")
	}
}
