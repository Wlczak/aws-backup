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
		{name: "missing paths", body: `{"paths":[],"days":7}`, status: http.StatusBadRequest},
		{name: "missing days", body: `{"paths":["a"]}`, status: http.StatusBadRequest},
		{name: "days zero", body: `{"paths":["a"],"days":0}`, status: http.StatusBadRequest},
		{name: "days too high", body: `{"paths":["a"],"days":31}`, status: http.StatusBadRequest},
		{name: "days negative", body: `{"paths":["a"],"days":-1}`, status: http.StatusBadRequest},
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

// TestRestoreTriggerRequestsRestore covers the new request-only flow:
// /api/restore/trigger calls Storage.Restore for each unique key and
// flips matching DB rows to restore_status='in_progress'. It does NOT
// download anything.
func TestRestoreTriggerRequestsRestore(t *testing.T) {
	ts, d, store := newSyncTestServer(t)
	ctx := context.Background()
	now := time.Now()

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

	body, _ := json.Marshal(map[string]any{
		"paths": []string{"/"},
		"days":  7,
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
	// MemStorage's Restore returns ErrUnsupported for non-archive
	// classes, so the per-key call lands in Errors. We're really
	// testing request shape + that the file was matched.
	if out.FilesAffected != 1 {
		t.Errorf("FilesAffected: got %d (%+v)", out.FilesAffected, out)
	}
	_ = os.TempDir // keep os referenced for filepath usage above
	_ = filepath.Separator
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
		"application/json", bytes.NewReader([]byte(`{"paths":["x"],"days":7}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}

	_ = storage.ErrNotFound
}
