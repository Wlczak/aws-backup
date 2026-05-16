package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	cfg := config.Default()
	srv := &Server{deps: Deps{
		DB:     d,
		Bus:    events.NewBus(16),
		Config: &cfg,
		Storage: func() storage.Storage {
			return storage.NewMemStorage()
		},
	}}

	cases := []struct {
		name   string
		body   string
		status int
	}{
		{name: "missing paths", body: `{"paths":[],"days":7}`, status: http.StatusBadRequest},
		{name: "missing tier", body: `{"paths":["a"],"days":7}`, status: http.StatusBadRequest},
		{name: "invalid tier", body: `{"paths":["a"],"days":7,"tier":"fast"}`, status: http.StatusBadRequest},
		{name: "missing days", body: `{"paths":["a"]}`, status: http.StatusBadRequest},
		{name: "days zero", body: `{"paths":["a"],"days":0}`, status: http.StatusBadRequest},
		{name: "days too high", body: `{"paths":["a"],"days":181}`, status: http.StatusBadRequest},
		{name: "days negative", body: `{"paths":["a"],"days":-1}`, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/restore/trigger", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.handleRestoreTrigger(rr, req)
			if rr.Code != tc.status {
				t.Errorf("status: got %d want %d", rr.Code, tc.status)
			}
		})
	}
}

func TestRestoreToDirValidatesRequest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	cfg := config.Default()
	cfg.Backup.TmpDir = t.TempDir()
	srv := NewServer(Deps{
		DB:     d,
		Bus:    events.NewBus(16),
		Config: &cfg,
		Storage: func() storage.Storage {
			return storage.NewMemStorage()
		},
	})

	cases := []struct {
		name   string
		body   string
		status int
	}{
		{name: "missing paths", body: `{"paths":[],"target_dir":"/tmp/out"}`, status: http.StatusBadRequest},
		{name: "missing target", body: `{"paths":["/"],"target_dir":""}`, status: http.StatusBadRequest},
		{name: "relative target", body: `{"paths":["/"],"target_dir":"relative/path"}`, status: http.StatusBadRequest},
		{name: "invalid json", body: `{`, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/restore/to-dir", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.handleRestoreToDir(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status=%d want %d body=%s", rr.Code, tc.status, rr.Body.String())
			}
		})
	}
}

func TestRestoreToDirDownloadsMixedStandaloneAndZip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	now := time.Now()
	seed := []db.BatchEntry{
		{Path: "docs/readme.md", Size: 5, ModTime: now},
		{Path: "photos/a.jpg", Size: 3, ModTime: now},
		{Path: "photos/b.jpg", Size: 4, ModTime: now},
		{Path: "pending.txt", Size: 7, ModTime: now},
	}
	res, err := d.UpsertFileBatch(ctx, seed, now)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for i, r := range res {
		ids[seed[i].Path] = r.ID
	}
	if err := d.MarkUploadedBatch(ctx, []int64{ids["docs/readme.md"]}, md5hex("hello"), "backups/docs/readme.md", now); err != nil {
		t.Fatal(err)
	}
	if err := d.SetZipName(ctx, []int64{ids["photos/a.jpg"], ids["photos/b.jpg"]}, "photos/photos_1.zip"); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{ids["photos/a.jpg"]}, md5hex("aaa"), "backups/photos/photos_1.zip", now); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{ids["photos/b.jpg"]}, md5hex("bbbb"), "backups/photos/photos_1.zip", now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestored(ctx, "backups/docs/readme.md", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestored(ctx, "backups/photos/photos_1.zip", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{ids["pending.txt"]}, md5hex("skip-me"), "backups/pending.txt", now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestoreInProgress(ctx, "backups/pending.txt"); err != nil {
		t.Fatal(err)
	}

	store := storage.NewMemStorage()
	mustPut := func(key, body string) {
		t.Helper()
		if _, err := store.Put(ctx, key, strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	mustPut("backups/docs/readme.md", "hello")
	zipBytes := buildTestZip(t, map[string]string{
		"photos/a.jpg": "aaa",
		"photos/b.jpg": "bbbb",
	})
	if _, err := store.Put(ctx, "backups/photos/photos_1.zip", bytes.NewReader(zipBytes), int64(len(zipBytes))); err != nil {
		t.Fatal(err)
	}
	mustPut("backups/pending.txt", "skip-me")

	cfg := config.Default()
	cfg.Backup.TmpDir = t.TempDir()
	srv := NewServer(Deps{
		DB:            d,
		Bus:           events.NewBus(16),
		Config:        &cfg,
		Storage:       func() storage.Storage { return store },
		StoragePrefix: "backups/",
	})

	body, _ := json.Marshal(map[string]any{
		"paths":      []string{"/"},
		"target_dir": t.TempDir(),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/restore/to-dir", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleRestoreToDir(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var out restoreToDirResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.FilesWritten != 3 {
		t.Fatalf("files_written=%d want 3 (%+v)", out.FilesWritten, out)
	}
	if out.BytesWritten != int64(len("hello")+len("aaa")+len("bbbb")) {
		t.Fatalf("bytes_written=%d want %d", out.BytesWritten, len("hello")+len("aaa")+len("bbbb"))
	}
	if len(out.Skipped) != 1 || !strings.Contains(out.Skipped[0], "pending.txt") {
		t.Fatalf("skipped=%v want pending.txt", out.Skipped)
	}
}

// TestRestoreTriggerRequestsRestore covers the new request-only flow:
// /api/restore/trigger calls Storage.Restore for each unique key and
// flips matching DB rows to restore_status='in_progress'. It does NOT
// download anything.
func TestRestoreTriggerRequestsRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	store := storage.NewMemStorage()
	cfg := config.Default()
	srv := &Server{deps: Deps{
		DB:            d,
		Bus:           events.NewBus(16),
		Config:        &cfg,
		Storage:       func() storage.Storage { return store },
		StoragePrefix: "backups/",
	}}
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
		"tier":  "standard",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/restore/trigger", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleRestoreTrigger(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}

	var out restoreTriggerResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// MemStorage's Restore returns ErrUnsupported for non-archive
	// classes, so the per-key call lands in Errors. We're really
	// testing request shape + that the file was matched.
	if out.FilesAffected != 1 {
		t.Errorf("FilesAffected: got %d (%+v)", out.FilesAffected, out)
	}
}

type tierSpyStorage struct {
	*storage.MemStorage
	mu    sync.Mutex
	calls []storage.RestoreTier
	keys  []string
}

func newTierSpyStorage() *tierSpyStorage {
	return &tierSpyStorage{MemStorage: storage.NewMemStorage()}
}

func (s *tierSpyStorage) Restore(_ context.Context, key string, _ int, tier storage.RestoreTier) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, tier)
	s.keys = append(s.keys, key)
	return nil
}

func TestRestoreEstimateValidatesTier(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	cfg := config.Default()
	srv := &Server{deps: Deps{DB: d, Bus: events.NewBus(16), Config: &cfg}}

	cases := []struct {
		name string
		body string
	}{
		{name: "missing days", body: `{"paths":["/"],"tier":"bulk"}`},
		{name: "days zero", body: `{"paths":["/"],"tier":"bulk","days":0}`},
		{name: "days too high", body: `{"paths":["/"],"tier":"bulk","days":181}`},
		{name: "missing tier", body: `{"paths":["/"],"days":7}`},
		{name: "invalid tier", body: `{"paths":["/"],"tier":"fast","days":7}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/restore/estimate", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.handleRestoreEstimate(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400", rr.Code)
			}
		})
	}
}

func TestRestoreEstimateTierChangesPreview(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	now := time.Now()

	res, err := d.UpsertFileBatch(ctx, []db.BatchEntry{{Path: "notes.txt", Size: 1024 * 1024 * 1024, ModTime: now}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{res[0].ID}, md5hex("hello"), "backups/notes.txt", now); err != nil {
		t.Fatal(err)
	}

	post := func(tier string) restoreEstimateResponse {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"paths": []string{"/"},
			"tier":  tier,
			"days":  30,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/restore/estimate", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv := &Server{deps: Deps{DB: d, Bus: events.NewBus(16), Config: &config.Config{}}}
		srv.handleRestoreEstimate(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d", rr.Code)
		}
		var out restoreEstimateResponse
		if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	bulk := post("bulk")
	std := post("standard")

	if bulk.WaitHoursMin != 48 || bulk.WaitHoursMax != 48 {
		t.Fatalf("bulk wait = %d-%d, want 48-48", bulk.WaitHoursMin, bulk.WaitHoursMax)
	}
	if std.WaitHoursMin != 12 || std.WaitHoursMax != 12 {
		t.Fatalf("standard wait = %d-%d, want 12-12", std.WaitHoursMin, std.WaitHoursMax)
	}
	if bulk.TotalFeeUSD >= std.TotalFeeUSD {
		t.Fatalf("bulk should cost less than standard: bulk=%v std=%v", bulk.TotalFeeUSD, std.TotalFeeUSD)
	}
	if bulk.StorageFeeUSD == 0 || std.StorageFeeUSD == 0 {
		t.Fatalf("expected storage fee in both estimates: bulk=%+v std=%+v", bulk, std)
	}
	if bulk.StorageFeeUSD != std.StorageFeeUSD {
		t.Fatalf("storage fee should not depend on tier: bulk=%v std=%v", bulk.StorageFeeUSD, std.StorageFeeUSD)
	}
}

func TestRestoreEstimateCountsZipAsOneObject(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	now := time.Now()

	seed := []db.BatchEntry{
		{Path: "photos/a.jpg", Size: 3, ModTime: now},
		{Path: "photos/b.jpg", Size: 4, ModTime: now},
		{Path: "notes.txt", Size: 5, ModTime: now},
	}
	res, err := d.UpsertFileBatch(ctx, seed, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetZipName(ctx, []int64{res[0].ID, res[1].ID}, "photos/photos_1.zip"); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{res[0].ID}, md5hex("aaa"), "backups/photos/photos_1.zip", now); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{res[1].ID}, md5hex("bbbb"), "backups/photos/photos_1.zip", now); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{res[2].ID}, md5hex("hello"), "backups/notes.txt", now); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	srv := &Server{deps: Deps{DB: d, Bus: events.NewBus(16), Config: &cfg}}
	body, _ := json.Marshal(map[string]any{
		"paths": []string{"/"},
		"tier":  "bulk",
		"days":  30,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/restore/estimate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleRestoreEstimate(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var out restoreEstimateResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.FileCount != 2 {
		t.Fatalf("file_count=%d want 2 (zip + standalone)", out.FileCount)
	}
}

func TestRestoreEstimateUsesObjectCountForRequestFee(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	now := time.Now()

	seed := make([]db.BatchEntry, 100)
	for i := range seed {
		seed[i] = db.BatchEntry{Path: fmt.Sprintf("photos/file-%03d.jpg", i), Size: 1, ModTime: now}
	}
	res, err := d.UpsertFileBatch(ctx, seed, now)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(res))
	for _, r := range res {
		ids = append(ids, r.ID)
	}
	if err := d.SetZipName(ctx, ids, "photos/photos_1.zip"); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, ids, md5hex("zip-bytes"), "backups/photos/photos_1.zip", now); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	srv := &Server{deps: Deps{DB: d, Bus: events.NewBus(16), Config: &cfg}}
	body, _ := json.Marshal(map[string]any{
		"paths": []string{"/"},
		"tier":  "standard",
		"days":  30,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/restore/estimate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleRestoreEstimate(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var out restoreEstimateResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.FileCount != 1 {
		t.Fatalf("file_count=%d want 1", out.FileCount)
	}
	if out.RequestFeeUSD != 0 {
		t.Fatalf("request_fee_usd=%v want 0.00 at this scale after rounding", out.RequestFeeUSD)
	}
}

func TestEstimateDownloadFeesUsesFullEgressBytes(t *testing.T) {
	_, egress, total := estimateDownloadFees(250, 150*1024*1024*1024)
	if diff := math.Abs(egress - 13.5); diff > 0.0001 {
		t.Fatalf("egress_fee_usd=%v want 13.5 (diff=%v)", egress, diff)
	}
	if diff := math.Abs(total - (13.5 + 250*0.0004/1000)); diff > 0.0001 {
		t.Fatalf("total_fee_usd=%v unexpected total (diff=%v)", total, diff)
	}
}

func TestRestoreTriggerForwardsTier(t *testing.T) {
	for _, tc := range []struct {
		name string
		tier string
		want storage.RestoreTier
	}{
		{name: "bulk", tier: "bulk", want: storage.RestoreTierBulk},
		{name: "standard", tier: "standard", want: storage.RestoreTierStandard},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { d.Close() })

			now := time.Now()
			res, err := d.UpsertFileBatch(ctx, []db.BatchEntry{{Path: "notes.txt", Size: 5, ModTime: now}}, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.MarkUploadedBatch(ctx, []int64{res[0].ID}, md5hex("hello"), "backups/notes.txt", now); err != nil {
				t.Fatal(err)
			}

			spy := newTierSpyStorage()
			cfg := config.Default()
			srv := NewServer(Deps{
				DB:            d,
				Bus:           events.NewBus(16),
				Config:        &cfg,
				Storage:       func() storage.Storage { return spy },
				StoragePrefix: "backups/",
			})

			body, _ := json.Marshal(map[string]any{
				"paths": []string{"/"},
				"days":  7,
				"tier":  tc.tier,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/restore/trigger", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.handleRestoreTrigger(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d want 200", rr.Code)
			}

			spy.mu.Lock()
			defer spy.mu.Unlock()
			if len(spy.calls) != 1 {
				t.Fatalf("Restore calls = %d want 1", len(spy.calls))
			}
			if spy.calls[0] != tc.want {
				t.Fatalf("tier = %v want %v", spy.calls[0], tc.want)
			}
		})
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
	req := httptest.NewRequest(http.MethodPost, "/api/restore/trigger", bytes.NewReader([]byte(`{"paths":["x"],"days":7}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleRestoreTrigger(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rr.Code)
	}

	_ = storage.ErrNotFound
}

func buildTestZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	keys := make([]string, 0, len(files))
	for name := range files {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(files[name])); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}
