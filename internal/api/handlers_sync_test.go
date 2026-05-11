package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/events"
	"github.com/Wlczak/aws-backup/internal/storage"
)

// newSyncTestServer wires a Server against a real DB + MemStorage so we
// can exercise /api/sync and /api/sync/full against known cloud state.
func newSyncTestServer(t *testing.T) (*httptest.Server, *db.DB, *storage.MemStorage) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	store := storage.NewMemStorage()
	cfg := config.Default()
	srv := NewServer(Deps{
		DB:            d,
		Bus:           events.NewBus(16),
		Config:        &cfg,
		Storage:       func() storage.Storage { return store },
		StoragePrefix: "backups/",
	})
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return ts, d, store
}

func TestSyncFullReportsLocalAndCloudDiffs(t *testing.T) {
	ts, d, store := newSyncTestServer(t)
	ctx := context.Background()
	now := time.Now()

	// Seed gone-locally.txt first with a past seenAt so MarkMissing can
	// target it by last_seen_at without affecting the other three files.
	pastTime := now.Add(-time.Hour)
	goneRes, err := d.UpsertFile(ctx, "gone-locally.txt", 1, pastTime, pastTime)
	if err != nil {
		t.Fatalf("seed gone-locally: %v", err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{goneRes.ID}, "md5", "backups/gone-locally.txt", pastTime); err != nil {
		t.Fatalf("mark gone uploaded: %v", err)
	}
	// MarkMissing flips rows whose last_seen_at < now; gone-locally qualifies.
	// s3_key is preserved so ListIndividualS3Keys still returns it.
	if _, err := d.MarkMissing(ctx, now); err != nil {
		t.Fatalf("mark missing: %v", err)
	}
	// Seed a second row that claims it was uploaded, but the object is
	// not actually present in S3. The merged sync should push it back to
	// pending too.
	goneCloudRes, err := d.UpsertFile(ctx, "gone-cloud.txt", 1, pastTime, pastTime)
	if err != nil {
		t.Fatalf("seed gone-cloud: %v", err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{goneCloudRes.ID}, "md5", "backups/gone-cloud.txt", pastTime); err != nil {
		t.Fatalf("mark gone-cloud uploaded: %v", err)
	}

	// Seed the remaining three files.
	seed := []db.BatchEntry{
		{Path: "photos/a.jpg", Size: 10, ModTime: now},
		{Path: "docs/spec.pdf", Size: 20, ModTime: now},
		{Path: "new-on-disk.txt", Size: 5, ModTime: now},
	}
	res, err := d.UpsertFileBatch(ctx, seed, now)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	ids := map[string]int64{}
	for i, r := range res {
		ids[seed[i].Path] = r.ID
	}

	// Mark photos/a.jpg as an individual upload.
	if err := d.MarkUploadedBatch(ctx, []int64{ids["photos/a.jpg"]}, "md5", "backups/photos/a.jpg", now); err != nil {
		t.Fatalf("mark photos uploaded: %v", err)
	}
	// Mark docs/spec.pdf as a zipped upload.
	if err := d.SetZipName(ctx, []int64{ids["docs/spec.pdf"]}, "docs/docs_1.zip"); err != nil {
		t.Fatalf("set zip name: %v", err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{ids["docs/spec.pdf"]}, "md5", "backups/docs/docs_1.zip", now); err != nil {
		t.Fatalf("mark docs uploaded: %v", err)
	}

	// Seed S3: the zip + its index, the standalone file, and one
	// cloud-only file (restore-me.jpg inside a zip) that has no local row.
	mustPut := func(key, body string) {
		t.Helper()
		if _, err := store.Put(ctx, key, strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	mustPut("backups/photos/a.jpg", "x")
	mustPut("backups/docs/docs_1.zip", "zipbytes")
	mustPut("backups/docs/docs_1.zip.index.txt", "docs/spec.pdf\nrestore-me.jpg\n")
	// Note: backups/gone-locally.txt is intentionally ABSENT to exercise
	// the existence-check reset path.

	resp, err := ts.Client().Post(ts.URL+"/api/sync/full", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}

	var body fullSyncResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Local file set = {photos/a.jpg, docs/spec.pdf, new-on-disk.txt,
	// gone-cloud.txt}; gone-locally is status=missing, so excluded.
	if body.LocalFileCount != 4 {
		t.Errorf("LocalFileCount: got %d want 4 (body=%+v)", body.LocalFileCount, body)
	}
	// Cloud index = {photos/a.jpg (standalone), docs/spec.pdf,
	// restore-me.jpg}.
	if body.CloudFileCount != 3 {
		t.Errorf("CloudFileCount: got %d want 3 (body=%+v)", body.CloudFileCount, body)
	}

	sort.Strings(body.LocalMissingFromCloud)
	if strings.Join(body.LocalMissingFromCloud, ",") != "gone-cloud.txt,new-on-disk.txt" {
		t.Errorf("LocalMissingFromCloud: got %v want [gone-cloud.txt new-on-disk.txt]", body.LocalMissingFromCloud)
	}

	sort.Strings(body.CloudMissingFromLocal)
	if strings.Join(body.CloudMissingFromLocal, ",") != "restore-me.jpg" {
		t.Errorf("CloudMissingFromLocal: got %v want [restore-me.jpg]", body.CloudMissingFromLocal)
	}

	if body.ZipIndexesConsumed != 1 {
		t.Errorf("ZipIndexesConsumed: got %d want 1", body.ZipIndexesConsumed)
	}

	// Only the row that is still local should be normalised back to
	// pending. The missing row stays missing even though its cloud object
	// is gone.
	if body.MissingIndividual < 2 {
		t.Errorf("expected two missing individual keys, got %d", body.MissingIndividual)
	}
	if body.FilesReset < 1 {
		t.Errorf("expected one row reset to pending, got %d", body.FilesReset)
	}

	goneLocallyRows, _, err := d.ListFiles(ctx, db.FilesFilter{Search: "gone-locally.txt", All: true})
	if err != nil {
		t.Fatalf("reload gone-locally: %v", err)
	}
	if len(goneLocallyRows) != 1 {
		t.Fatalf("reload gone-locally rows=%d want 1", len(goneLocallyRows))
	}
	goneCloudRows, _, err := d.ListFiles(ctx, db.FilesFilter{Search: "gone-cloud.txt", All: true})
	if err != nil {
		t.Fatalf("reload gone-cloud: %v", err)
	}
	if len(goneCloudRows) != 1 {
		t.Fatalf("reload gone-cloud rows=%d want 1", len(goneCloudRows))
	}
	if goneLocallyRows[0].Status != db.StatusPending {
		t.Errorf("gone-locally status=%q want pending", goneLocallyRows[0].Status)
	}
	if goneCloudRows[0].Status != db.StatusMissing {
		t.Errorf("gone-cloud status=%q want missing", goneCloudRows[0].Status)
	}
}

func TestSyncFullStorageNotConfigured(t *testing.T) {
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

	resp, err := ts.Client().Post(ts.URL+"/api/sync/full", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}
