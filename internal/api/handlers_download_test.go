package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/storage"
)

func TestDownloadFullCreatesAndReusesMirrorSnapshot(t *testing.T) {
	ctx := context.Background()
	_, deps := newTestServer(t)
	store := storage.NewMemStorage()
	deps.Storage = func() storage.Storage { return store }
	srv := NewServer(deps)
	cfg := deps.Config
	mirrorDir := filepath.Join(t.TempDir(), "mirror")
	tmpDir := filepath.Join(t.TempDir(), "tmp")
	cfg.Backup.DownloadDir = mirrorDir
	cfg.Backup.TmpDir = tmpDir
	if err := os.MkdirAll(mirrorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	row, err := deps.DB.UpsertFile(ctx, "docs/readme.txt", int64(len("hello")), now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.DB.MarkUploaded(ctx, row.ID, hexMD5("hello"), "backups/docs/readme.txt", now); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mirrorDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirrorDir, "docs", "readme.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/download/full", nil)
	rr := httptest.NewRecorder()
	srv.handleDownloadFull(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("download full status=%d want 202", rr.Code)
	}
	srv.downloadWg.Wait()

	var status struct {
		DownloadSnapshot *downloadMirrorSnapshotSummary `json:"download_mirror_snapshot"`
		DownloadLast     *downloadSummary               `json:"download_last"`
	}
	statusReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	statusRR := httptest.NewRecorder()
	srv.handleStatus(statusRR, statusReq)
	if err := json.NewDecoder(statusRR.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.DownloadLast == nil {
		t.Fatal("download did not finish")
	}
	if status.DownloadSnapshot == nil {
		t.Fatal("status did not expose a mirror snapshot")
	}

	snap1, found, err := deps.DB.GetDownloadMirrorSnapshot(ctx, mirrorDir)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("snapshot row not created")
	}
	if snap1.TotalCount != 1 || snap1.PresentCount != 1 || snap1.MissingCount != 0 {
		t.Fatalf("snapshot counts = %+v", snap1)
	}

	files, _, err := deps.DB.ListFiles(ctx, db.FilesFilter{Search: "docs/readme.txt", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files=%d want 1", len(files))
	}
	firstCheckedAt := files[0].DownloadCheckedAt
	if firstCheckedAt == nil {
		t.Fatal("download_checked_at not set after initial scan")
	}

	time.Sleep(25 * time.Millisecond)
	req = httptest.NewRequest(http.MethodPost, "/api/download/full", nil)
	rr = httptest.NewRecorder()
	srv.handleDownloadFull(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("rerun status=%d want 202", rr.Code)
	}
	srv.downloadWg.Wait()

	statusReq = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	statusRR = httptest.NewRecorder()
	srv.handleStatus(statusRR, statusReq)
	if err := json.NewDecoder(statusRR.Body).Decode(&status); err != nil {
		t.Fatalf("decode status rerun: %v", err)
	}
	if status.DownloadLast == nil || status.DownloadLast.ID < 2 {
		t.Fatalf("rerun did not finish: %+v", status.DownloadLast)
	}

	files, _, err = deps.DB.ListFiles(ctx, db.FilesFilter{Search: "docs/readme.txt", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if files[0].DownloadCheckedAt == nil || !files[0].DownloadCheckedAt.Equal(*firstCheckedAt) {
		t.Fatalf("rerun updated download_checked_at unexpectedly: got %v want %v", files[0].DownloadCheckedAt, firstCheckedAt)
	}
	snap2, found, err := deps.DB.GetDownloadMirrorSnapshot(ctx, mirrorDir)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("snapshot disappeared")
	}
	if !snap2.ScannedAt.Equal(snap1.ScannedAt) {
		t.Fatalf("cached snapshot should be reused, got scanned_at=%v want %v", snap2.ScannedAt, snap1.ScannedAt)
	}
}

func TestDownloadRescanUpdatesMirrorSnapshot(t *testing.T) {
	ctx := context.Background()
	_, deps := newTestServer(t)
	store := storage.NewMemStorage()
	deps.Storage = func() storage.Storage { return store }
	srv := NewServer(deps)
	cfg := deps.Config
	mirrorDir := filepath.Join(t.TempDir(), "mirror")
	tmpDir := filepath.Join(t.TempDir(), "tmp")
	cfg.Backup.DownloadDir = mirrorDir
	cfg.Backup.TmpDir = tmpDir
	if err := os.MkdirAll(filepath.Join(mirrorDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	row, err := deps.DB.UpsertFile(ctx, "docs/readme.txt", int64(len("hello")), now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.DB.MarkUploaded(ctx, row.ID, hexMD5("hello"), "backups/docs/readme.txt", now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirrorDir, "docs", "readme.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/download/full", nil)
	rr := httptest.NewRecorder()
	srv.handleDownloadFull(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d want 202", rr.Code)
	}
	srv.downloadWg.Wait()

	snap1, found, err := deps.DB.GetDownloadMirrorSnapshot(ctx, mirrorDir)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("initial snapshot missing")
	}
	files, _, err := deps.DB.ListFiles(ctx, db.FilesFilter{Search: "docs/readme.txt", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].DownloadCheckedAt == nil {
		t.Fatalf("download metadata missing after bootstrap: %+v", files)
	}

	time.Sleep(1100 * time.Millisecond)
	req = httptest.NewRequest(http.MethodPost, "/api/download/rescan", nil)
	rr = httptest.NewRecorder()
	srv.handleDownloadRescan(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("rescan status=%d want 202", rr.Code)
	}
	srv.downloadWg.Wait()

	snap2, found, err := deps.DB.GetDownloadMirrorSnapshot(ctx, mirrorDir)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("snapshot missing after rescan")
	}
	if !snap2.ScannedAt.After(snap1.ScannedAt) {
		t.Fatalf("rescan did not refresh scanned_at: before=%v after=%v", snap1.ScannedAt, snap2.ScannedAt)
	}
	files, _, err = deps.DB.ListFiles(ctx, db.FilesFilter{Search: "docs/readme.txt", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if files[0].DownloadCheckedAt == nil || !files[0].DownloadCheckedAt.After(snap1.ScannedAt) {
		t.Fatalf("rescan did not refresh file metadata: %+v", files[0].DownloadCheckedAt)
	}
}

func TestDownloadMirrorRoutesRejectWhenBusy(t *testing.T) {
	_, deps := newTestServer(t)
	store := storage.NewMemStorage()
	deps.Storage = func() storage.Storage { return store }
	srv := NewServer(deps)
	srv.downloadMu.Lock()
	srv.currentDownload = &downloadSummary{
		ID:        99,
		StartedAt: time.Now().UTC(),
		Status:    "running",
		Phase:     "scan",
	}
	srv.downloadMu.Unlock()
	for _, path := range []string{"/api/download/full", "/api/download/rescan"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rr := httptest.NewRecorder()
		switch path {
		case "/api/download/full":
			srv.handleDownloadFull(rr, req)
		default:
			srv.handleDownloadRescan(rr, req)
		}
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s status=%d want 409", path, rr.Code)
		}
	}
}

func TestDownloadCancelRequestsActiveJob(t *testing.T) {
	_, deps := newTestServer(t)
	srv := NewServer(deps)
	cancelled := false
	srv.downloadMu.Lock()
	srv.currentDownload = &downloadSummary{
		ID:        7,
		StartedAt: time.Now().UTC(),
		Status:    "running",
		Phase:     "download",
	}
	srv.currentDownloadCancel = func() {
		cancelled = true
	}
	srv.downloadMu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/download/cancel", nil)
	rr := httptest.NewRecorder()
	srv.handleDownloadCancel(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d want 202", rr.Code)
	}
	if !cancelled {
		t.Fatal("cancel function was not called")
	}
	if !srv.downloadCancelReq.Load() {
		t.Fatal("cancel flag was not raised")
	}
}

func TestDownloadCancelRejectsWhenIdle(t *testing.T) {
	_, deps := newTestServer(t)
	srv := NewServer(deps)

	req := httptest.NewRequest(http.MethodPost, "/api/download/cancel", nil)
	rr := httptest.NewRecorder()
	srv.handleDownloadCancel(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rr.Code)
	}
}
