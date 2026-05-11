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
func newSyncTestServer(t *testing.T) (*Server, *db.DB, *storage.MemStorage) {
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
	return srv, d, store
}

func TestSyncFullReportsLocalAndCloudDiffs(t *testing.T) {
	srv, d, store := newSyncTestServer(t)
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
	// Seed a row that is marked missing but whose object is still in S3.
	// Full sync should rebuild it as cloud_only because the bucket still
	// has the object.
	recoverableRes, err := d.UpsertFile(ctx, "recoverable.txt", 1, pastTime, pastTime)
	if err != nil {
		t.Fatalf("seed recoverable: %v", err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{recoverableRes.ID}, "md5", "backups/recoverable.txt", pastTime); err != nil {
		t.Fatalf("mark recoverable uploaded: %v", err)
	}
	if _, err := d.MarkMissingByPaths(ctx, []string{"recoverable.txt"}); err != nil {
		t.Fatalf("mark recoverable missing: %v", err)
	}

	// Seed the remaining three files.
	seed := []db.BatchEntry{
		{Path: "photos/a.jpg", Size: 10, ModTime: now},
		{Path: "docs/spec.pdf", Size: 20, ModTime: now},
		{Path: "new-on-disk.txt", Size: 5, ModTime: now},
		{Path: "pending-on-s3.txt", Size: 7, ModTime: now},
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
	// cloud-only file (restore-me.jpg inside a zip) that has no local row
	// and should be recreated as a cloud_only row. pending-on-s3.txt is
	// local-only before sync, but because it is in S3 it should be promoted
	// to uploaded. recoverable.txt exists in the DB as missing and should
	// be rebuilt as cloud_only because S3 still has the object.
	mustPut := func(key, body string) {
		t.Helper()
		if _, err := store.Put(ctx, key, strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	mustPut("backups/photos/a.jpg", "x")
	mustPut("backups/docs/docs_1.zip", "zipbytes")
	mustPut("backups/docs/docs_1.zip.index.txt", "docs/spec.pdf\nrestore-me.jpg\n")
	mustPut("backups/pending-on-s3.txt", "pending")
	mustPut("backups/recoverable.txt", "recoverable")
	// Note: backups/gone-locally.txt is intentionally ABSENT to exercise
	// the existence-check reset path.

	req := httptest.NewRequest(http.MethodPost, "/api/sync/full", nil)
	rr := httptest.NewRecorder()
	srv.handleSyncFull(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}

	var body fullSyncResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Local file set = {photos/a.jpg, docs/spec.pdf, new-on-disk.txt,
	// gone-cloud.txt, pending-on-s3.txt}; gone-locally and recoverable are
	// status=missing, so excluded.
	if body.LocalFileCount != 5 {
		t.Errorf("LocalFileCount: got %d want 5 (body=%+v)", body.LocalFileCount, body)
	}
	// Cloud index = {photos/a.jpg (standalone), docs/spec.pdf,
	// restore-me.jpg, pending-on-s3.txt, recoverable.txt}.
	if body.CloudFileCount != 5 {
		t.Errorf("CloudFileCount: got %d want 5 (body=%+v)", body.CloudFileCount, body)
	}

	sort.Strings(body.LocalMissingFromCloud)
	if strings.Join(body.LocalMissingFromCloud, ",") != "gone-cloud.txt,new-on-disk.txt" {
		t.Errorf("LocalMissingFromCloud: got %v want [gone-cloud.txt new-on-disk.txt]", body.LocalMissingFromCloud)
	}

	sort.Strings(body.CloudMissingFromLocal)
	if strings.Join(body.CloudMissingFromLocal, ",") != "recoverable.txt,restore-me.jpg" {
		t.Errorf("CloudMissingFromLocal: got %v want [recoverable.txt restore-me.jpg]", body.CloudMissingFromLocal)
	}

	if body.ZipIndexesConsumed != 1 {
		t.Errorf("ZipIndexesConsumed: got %d want 1", body.ZipIndexesConsumed)
	}
	if body.FilesCreated < 1 {
		t.Errorf("expected at least one recreated file, got %d", body.FilesCreated)
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
	if goneCloudRows[0].Status != db.StatusPending {
		t.Errorf("gone-cloud status=%q want pending", goneCloudRows[0].Status)
	}

	pendingOnS3Rows, _, err := d.ListFiles(ctx, db.FilesFilter{Search: "pending-on-s3.txt", All: true})
	if err != nil {
		t.Fatalf("reload pending-on-s3: %v", err)
	}
	if len(pendingOnS3Rows) != 1 {
		t.Fatalf("reload pending-on-s3 rows=%d want 1", len(pendingOnS3Rows))
	}
	if pendingOnS3Rows[0].Status != db.StatusUploaded {
		t.Errorf("pending-on-s3 status=%q want uploaded", pendingOnS3Rows[0].Status)
	}

	recoverableRows, _, err := d.ListFiles(ctx, db.FilesFilter{Search: "recoverable.txt", All: true})
	if err != nil {
		t.Fatalf("reload recoverable: %v", err)
	}
	if len(recoverableRows) != 1 {
		t.Fatalf("reload recoverable rows=%d want 1", len(recoverableRows))
	}
	if recoverableRows[0].Status != db.StatusCloudOnly {
		t.Errorf("recoverable status=%q want cloud_only", recoverableRows[0].Status)
	}

	restoreRows, _, err := d.ListFiles(ctx, db.FilesFilter{Search: "restore-me.jpg", All: true})
	if err != nil {
		t.Fatalf("reload restore-me: %v", err)
	}
	if len(restoreRows) != 1 {
		t.Fatalf("reload restore-me rows=%d want 1", len(restoreRows))
	}
	if restoreRows[0].Status != db.StatusCloudOnly {
		t.Errorf("restore-me status=%q want cloud_only", restoreRows[0].Status)
	}
	if restoreRows[0].ZipName != "docs/docs_1.zip" {
		t.Errorf("restore-me zip=%q want docs/docs_1.zip", restoreRows[0].ZipName)
	}
	if restoreRows[0].S3Key != "backups/docs/docs_1.zip" {
		t.Errorf("restore-me s3_key=%q want backups/docs/docs_1.zip", restoreRows[0].S3Key)
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

	req := httptest.NewRequest(http.MethodPost, "/api/sync/full", nil)
	rr := httptest.NewRecorder()
	srv.handleSyncFull(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rr.Code)
	}
}
