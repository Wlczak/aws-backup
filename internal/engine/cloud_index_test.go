package engine

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/Wlczak/aws-backup/internal/storage"
)

func TestLoadCloudIndex(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemStorage()

	// Two zip archives under the "backups/" prefix, each with a sidecar
	// index listing its entries. Also two standalone keys.
	mustPut(t, mem, "backups/photos/2024/photos_2024_1.zip", "binaryzipdata-1")
	mustPut(t, mem, "backups/photos/2024/photos_2024_1.zip.index.txt",
		"photos/2024/a.jpg\nphotos/2024/b.jpg\n")
	mustPut(t, mem, "backups/docs/docs_1.zip", "binaryzipdata-2")
	mustPut(t, mem, "backups/docs/docs_1.zip.index.txt",
		"docs/readme.md\ndocs/spec.pdf\n")
	mustPut(t, mem, "backups/loose/notes.txt", "raw individual")
	mustPut(t, mem, "backups/root.txt", "root individual")

	idx, err := LoadCloudIndex(ctx, mem, "backups/", []string{"backups/loose/notes.txt"})
	if err != nil {
		t.Fatalf("LoadCloudIndex: %v", err)
	}

	wantPaths := []string{
		"docs/readme.md",
		"docs/spec.pdf",
		"loose/notes.txt",
		"photos/2024/a.jpg",
		"photos/2024/b.jpg",
		"root.txt",
	}
	got := make([]string, 0, len(idx.Files))
	for p := range idx.Files {
		got = append(got, p)
	}
	sort.Strings(got)
	sort.Strings(wantPaths)
	if strings.Join(got, ",") != strings.Join(wantPaths, ",") {
		t.Fatalf("paths: got %v want %v", got, wantPaths)
	}

	if idx.IndexCount != 2 {
		t.Errorf("IndexCount: got %d want 2", idx.IndexCount)
	}
	if idx.ZipCount != 2 {
		t.Errorf("ZipCount: got %d want 2", idx.ZipCount)
	}
	if idx.Standalone != 2 {
		t.Errorf("Standalone: got %d want 2", idx.Standalone)
	}

	if z := idx.Files["photos/2024/a.jpg"].ZipKey; z != "backups/photos/2024/photos_2024_1.zip" {
		t.Errorf("zip key mismatch: %q", z)
	}
	if f := idx.Files["loose/notes.txt"]; f.S3Key != "backups/loose/notes.txt" || f.ZipKey != "" {
		t.Errorf("standalone record: %+v", f)
	}
	if f := idx.Files["root.txt"]; f.S3Key != "backups/root.txt" || f.ZipKey != "" {
		t.Errorf("root standalone record: %+v", f)
	}
}

func TestLoadCloudIndexEmpty(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemStorage()
	idx, err := LoadCloudIndex(ctx, mem, "backups/", nil)
	if err != nil {
		t.Fatalf("LoadCloudIndex: %v", err)
	}
	if len(idx.Files) != 0 || idx.IndexCount != 0 || idx.Standalone != 0 {
		t.Errorf("expected empty index, got %+v", idx)
	}
}

func mustPut(t *testing.T, mem *storage.MemStorage, key, body string) {
	t.Helper()
	_, err := mem.Put(context.Background(), key, strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}
