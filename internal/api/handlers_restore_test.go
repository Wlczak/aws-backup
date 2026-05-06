package api

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/events"
	"github.com/Wlczak/aws-backup/internal/storage"
)

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestRestoreTriggerValidatesRequest(t *testing.T) {
	ts, _, _ := newSyncTestServer(t)

	cases := []struct {
		name   string
		body   string
		status int
	}{
		{name: "missing paths", body: `{"paths":[],"target_dir":"/tmp"}`, status: http.StatusBadRequest},
		{name: "missing target", body: `{"paths":["a"]}`, status: http.StatusBadRequest},
		{name: "relative target", body: `{"paths":["a"],"target_dir":"relative/path"}`, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := ts.Client().Post(ts.URL+"/api/restore/trigger",
				"application/json", bytes.NewReader([]byte(tc.body)))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Errorf("status: got %d want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

func TestRestoreTriggerDownloadsStandalone(t *testing.T) {
	ts, d, store := newSyncTestServer(t)
	ctx := context.Background()
	now := time.Now()

	// Seed a single standalone file and its S3 object.
	res, err := d.UpsertFileBatch(ctx, []db.BatchEntry{{Path: "notes.txt", Size: 5, ModTime: now}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{res[0].ID}, md5hex("hello"), "backups/notes.txt", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, "backups/notes.txt",
		strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	body, _ := json.Marshal(map[string]any{
		"paths":      []string{"/"},
		"target_dir": target,
	})
	resp, err := ts.Client().Post(ts.URL+"/api/restore/trigger",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}

	var out restoreTriggerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.FilesWritten != 1 {
		t.Errorf("FilesWritten: got %d (%+v)", out.FilesWritten, out)
	}

	got, err := os.ReadFile(filepath.Join(target, "notes.txt"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("restored content: got %q want %q", got, "hello")
	}
}

func TestRestoreTriggerStorageNotConfigured(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	cfg := config.Default()
	srv := NewServer(Deps{
		DB:     d,
		Bus:    events.NewBus(16),
		Config: &cfg,
	})
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	resp, err := ts.Client().Post(ts.URL+"/api/restore/trigger",
		"application/json", bytes.NewReader([]byte(`{"paths":["x"],"target_dir":"/tmp"}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}

	// silence unused-import warnings if helpers were pruned
	_ = storage.ErrNotFound
}
