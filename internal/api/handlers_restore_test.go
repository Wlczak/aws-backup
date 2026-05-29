package api

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/events"
	"github.com/Wlczak/aws-backup/internal/restore/inventory"
	"github.com/Wlczak/aws-backup/internal/restore/scanner"
	"github.com/Wlczak/aws-backup/internal/storage"
	"log/slog"
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
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var out struct {
		RestoreDownloadID int64 `json:"restore_download_id"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RestoreDownloadID == 0 {
		t.Fatalf("restore_download_id missing in %+v", out)
	}

	deadline := time.Now().Add(2 * time.Second)
	var final *restoreDownloadSummary
	for time.Now().Before(deadline) {
		srv.restoreDownloadMu.Lock()
		if srv.lastRestoreDownload != nil {
			copy := *srv.lastRestoreDownload
			final = &copy
			srv.restoreDownloadMu.Unlock()
			break
		}
		srv.restoreDownloadMu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if final == nil {
		t.Fatal("restore download did not finish")
	}
	if final.FilesWritten != 3 {
		t.Fatalf("files_written=%d want 3 (%+v)", final.FilesWritten, final)
	}
	if final.BytesWritten != int64(len("hello")+len("aaa")+len("bbbb")) {
		t.Fatalf("bytes_written=%d want %d", final.BytesWritten, len("hello")+len("aaa")+len("bbbb"))
	}
}

func TestRestoreDownloadStatusTracksCurrentFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	now := time.Now()
	seed := []db.BatchEntry{{Path: "big.bin", Size: 5, ModTime: now}}
	res, err := d.UpsertFileBatch(ctx, seed, now)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := d.MarkUploaded(ctx, res[0].ID, md5hex("abcde"), "backups/big.bin", now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRestored(ctx, "backups/big.bin", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	store := storage.NewMemStorage()
	if _, err := store.Put(ctx, "backups/big.bin", strings.NewReader("abcde"), 5); err != nil {
		t.Fatal(err)
	}
	slow := &slowStatusStorage{Storage: store}

	cfg := config.Default()
	cfg.Backup.TmpDir = t.TempDir()
	srv := NewServer(Deps{
		DB:      d,
		Bus:     events.NewBus(16),
		Config:  &cfg,
		Storage: func() storage.Storage { return slow },
	})

	body, _ := json.Marshal(map[string]any{
		"paths":      []string{"/"},
		"target_dir": t.TempDir(),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/restore/to-dir", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleRestoreToDir(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	var cur *restoreDownloadSummary
	for time.Now().Before(deadline) {
		srv.restoreDownloadMu.Lock()
		if srv.currentRestoreDownload != nil && srv.currentRestoreDownload.CurrentPath != "" && srv.currentRestoreDownload.CurrentBytes > 0 {
			copy := *srv.currentRestoreDownload
			cur = &copy
			srv.restoreDownloadMu.Unlock()
			break
		}
		srv.restoreDownloadMu.Unlock()
		time.Sleep(25 * time.Millisecond)
	}
	if cur == nil {
		t.Fatal("current restore download did not surface byte progress")
	}
	if cur.CurrentPath != "big.bin" {
		t.Fatalf("current path = %q want big.bin", cur.CurrentPath)
	}
	if cur.CurrentTotalBytes != 5 {
		t.Fatalf("current total bytes = %d want 5", cur.CurrentTotalBytes)
	}
	if cur.CurrentPercent <= 0 || cur.CurrentPercent >= 100 {
		t.Fatalf("current percent = %d want in-flight progress", cur.CurrentPercent)
	}
}

type slowStatusStorage struct {
	storage.Storage
}

func (s *slowStatusStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.Storage.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return &slowStatusReadCloser{ReadCloser: rc}, nil
}

type slowStatusReadCloser struct {
	io.ReadCloser
}

func (s *slowStatusReadCloser) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	buf := make([]byte, 1)
	n, err := s.ReadCloser.Read(buf)
	if n > 0 {
		p[0] = buf[0]
		time.Sleep(300 * time.Millisecond)
		return 1, nil
	}
	return 0, err
}

// TestRestoreTriggerRequestsRestore covers the async job flow:
// /api/restore/trigger returns 202 immediately, then the background job
// calls Storage.Restore for each unique key and flips matching DB rows
// to restore_status='in_progress'. It does NOT download anything.
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
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status: %d", rr.Code)
	}

	var out restoreJobStartResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RestoreJobID == 0 {
		t.Fatalf("restore_job_id missing in %+v", out)
	}

	srv.restoreJobWg.Wait()
	statusReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	statusRR := httptest.NewRecorder()
	srv.handleStatus(statusRR, statusReq)
	var status statusResponse
	if err := json.NewDecoder(statusRR.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.RestoreJobLast == nil {
		t.Fatal("restore job did not finish")
	}
	if status.RestoreJobLast.Status != "completed" {
		t.Fatalf("status=%q want completed", status.RestoreJobLast.Status)
	}
	if status.RestoreJobLast.KeysRequested != 1 || status.RestoreJobLast.FilesAffected != 1 {
		t.Fatalf("job summary = %+v", status.RestoreJobLast)
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

type blockingRestoreStorage struct {
	*storage.MemStorage
	startOnce sync.Once
	started   chan struct{}
	release   chan struct{}
}

func newBlockingRestoreStorage() *blockingRestoreStorage {
	return &blockingRestoreStorage{
		MemStorage: storage.NewMemStorage(),
		started:    make(chan struct{}),
		release:    make(chan struct{}),
	}
}

func (s *blockingRestoreStorage) Restore(ctx context.Context, key string, days int, tier storage.RestoreTier) error {
	s.startOnce.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return s.MemStorage.Restore(ctx, key, days, tier)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *blockingRestoreStorage) releaseRestore() {
	close(s.release)
}

type fakeInventoryAPI struct {
	bucket          string
	manifestKey     string
	dataKey         string
	manifestBody    []byte
	manifestHashHex string
	dataBody        []byte
	startOnce       sync.Once
	started         chan struct{}
	release         chan struct{}
}

func (f *fakeInventoryAPI) GetBucketInventoryConfiguration(_ context.Context, _ *s3.GetBucketInventoryConfigurationInput, _ ...func(*s3.Options)) (*s3.GetBucketInventoryConfigurationOutput, error) {
	return &s3.GetBucketInventoryConfigurationOutput{
		InventoryConfiguration: &s3types.InventoryConfiguration{
			Id:        aws.String(inventory.ConfigID),
			IsEnabled: aws.Bool(true),
			Schedule: &s3types.InventorySchedule{
				Frequency: s3types.InventoryFrequencyDaily,
			},
			Destination: &s3types.InventoryDestination{
				S3BucketDestination: &s3types.InventoryS3BucketDestination{
					Bucket: aws.String("arn:aws:s3:::" + f.bucket),
					Format: s3types.InventoryFormatCsv,
					Prefix: aws.String(strings.TrimSuffix(inventory.DestinationPrefix, "/")),
				},
			},
		},
	}, nil
}

func (f *fakeInventoryAPI) PutBucketInventoryConfiguration(_ context.Context, _ *s3.PutBucketInventoryConfigurationInput, _ ...func(*s3.Options)) (*s3.PutBucketInventoryConfigurationOutput, error) {
	return &s3.PutBucketInventoryConfigurationOutput{}, nil
}

func (f *fakeInventoryAPI) DeleteBucketInventoryConfiguration(_ context.Context, _ *s3.DeleteBucketInventoryConfigurationInput, _ ...func(*s3.Options)) (*s3.DeleteBucketInventoryConfigurationOutput, error) {
	return &s3.DeleteBucketInventoryConfigurationOutput{}, nil
}

func (f *fakeInventoryAPI) ListObjectsV2(_ context.Context, _ *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	modified := time.Now().Add(-10 * time.Minute)
	return &s3.ListObjectsV2Output{
		Contents: []s3types.Object{
			{Key: aws.String(f.manifestKey), LastModified: aws.Time(modified)},
		},
	}, nil
}

func (f *fakeInventoryAPI) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	key := aws.ToString(in.Key)
	if key == f.manifestKey {
		f.startOnce.Do(func() { close(f.started) })
		select {
		case <-f.release:
		}
		return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(f.manifestBody))}, nil
	}
	if key == strings.TrimSuffix(f.manifestKey, "manifest.json")+"manifest.checksum" {
		return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(f.manifestHashHex))}, nil
	}
	if key == f.dataKey {
		return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(f.dataBody))}, nil
	}
	return nil, fmt.Errorf("unexpected key %q", key)
}

func buildInventoryData(t *testing.T, bucket, dataKey string) (manifestBody []byte, manifestHashHex string, dataBody []byte) {
	t.Helper()
	var csvBuf bytes.Buffer
	cw := csv.NewWriter(&csvBuf)
	if err := cw.Write([]string{bucket, dataKey, "1", "deadbeef"}); err != nil {
		t.Fatal(err)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		t.Fatal(err)
	}
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(csvBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	dataBody = append([]byte(nil), gzBuf.Bytes()...)
	dataHash := md5.Sum(dataBody)
	dataHashHex := hex.EncodeToString(dataHash[:])
	mf := map[string]any{
		"sourceBucket": bucket,
		"fileFormat":   "CSV",
		"fileSchema":   "Bucket, Key, Size, MD5checksum",
		"files": []map[string]any{{
			"key":         "data.csv.gz",
			"size":        len(dataBody),
			"MD5checksum": dataHashHex,
		}},
	}
	manifestBody, err := json.Marshal(mf)
	if err != nil {
		t.Fatal(err)
	}
	manifestHash := md5.Sum(manifestBody)
	manifestHashHex = hex.EncodeToString(manifestHash[:])
	return manifestBody, manifestHashHex, dataBody
}

func TestInventorySyncIgnoresRequestCancel(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	bucket := "bucket"
	dataKey := "docs/readme.txt"
	now := time.Now()
	res, err := d.UpsertFileBatch(ctx, []db.BatchEntry{{Path: dataKey, Size: 5, ModTime: now}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{res[0].ID}, md5hex("hello"), "backups/"+dataKey, now); err != nil {
		t.Fatal(err)
	}

	store := storage.NewMemStorage()
	if _, err := store.Put(ctx, "backups/"+dataKey, strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}

	manifestKey := "_inventory/" + bucket + "/" + inventory.ConfigID + "/2026-01-01T00-00Z/manifest.json"
	manifestBody, manifestHashHex, dataBody := buildInventoryData(t, bucket, dataKey)
	api := &fakeInventoryAPI{
		bucket:          bucket,
		manifestKey:     manifestKey,
		dataKey:         "data.csv.gz",
		manifestBody:    manifestBody,
		manifestHashHex: manifestHashHex,
		dataBody:        dataBody,
		started:         make(chan struct{}),
		release:         make(chan struct{}),
	}
	inv := inventory.New(func() (inventory.API, string, bool) { return api, bucket, true }, "")
	srv := &Server{deps: Deps{
		DB:             d,
		Bus:            events.NewBus(16),
		Config:         &config.Config{},
		Storage:        func() storage.Storage { return store },
		Inventory:      inv,
		RestoreScanner: scanner.New(d, func() storage.Storage { return store }, events.NewBus(16), slog.Default()),
	}}

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	req := httptest.NewRequestWithContext(reqCtx, http.MethodPost, "/api/restore/inventory/sync", nil)
	rr := httptest.NewRecorder()
	srv.handleInventorySync(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d want 202", rr.Code)
	}
	cancelReq()
	select {
	case <-api.started:
	case <-time.After(2 * time.Second):
		t.Fatal("inventory sync did not start")
	}
	srv.restoreJobMu.Lock()
	if srv.currentRestoreJob == nil {
		srv.restoreJobMu.Unlock()
		t.Fatal("inventory job cleared before release")
	}
	srv.restoreJobMu.Unlock()
	close(api.release)
	srv.restoreJobWg.Wait()
	srv.restoreJobMu.Lock()
	defer srv.restoreJobMu.Unlock()
	if srv.currentRestoreJob != nil {
		t.Fatalf("current inventory job still active: %+v", srv.currentRestoreJob)
	}
	if srv.lastRestoreJob == nil || srv.lastRestoreJob.Status != "completed" {
		t.Fatalf("inventory job did not complete: %+v", srv.lastRestoreJob)
	}
	if srv.lastRestoreJob.Kind != restoreJobKindInventory {
		t.Fatalf("kind=%q want inventory", srv.lastRestoreJob.Kind)
	}
}

func TestRestoreTriggerIgnoresRequestCancel(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	store := newBlockingRestoreStorage()
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
	if _, err := store.Put(ctx, "backups/notes.txt", strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	body, _ := json.Marshal(map[string]any{"paths": []string{"/"}, "days": 7, "tier": "standard"})
	req := httptest.NewRequestWithContext(reqCtx, http.MethodPost, "/api/restore/trigger", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleRestoreTrigger(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d want 202", rr.Code)
	}
	cancelReq()
	select {
	case <-store.started:
	case <-time.After(2 * time.Second):
		t.Fatal("restore job never started")
	}

	srv.restoreJobMu.Lock()
	if srv.currentRestoreJob == nil {
		srv.restoreJobMu.Unlock()
		t.Fatal("restore job cleared before release")
	}
	srv.restoreJobMu.Unlock()

	store.releaseRestore()
	srv.restoreJobWg.Wait()
	srv.restoreJobMu.Lock()
	defer srv.restoreJobMu.Unlock()
	if srv.currentRestoreJob != nil {
		t.Fatalf("current restore job still active: %+v", srv.currentRestoreJob)
	}
	if srv.lastRestoreJob == nil || srv.lastRestoreJob.Status != "completed" {
		t.Fatalf("restore job did not complete: %+v", srv.lastRestoreJob)
	}
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

func TestRestoreEstimateIgnoresDBSnapshotPath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	now := time.Now()

	res, err := d.UpsertFileBatch(ctx, []db.BatchEntry{
		{Path: db.ReservedSnapshotPath, Size: 111, ModTime: now},
		{Path: "docs/report.pdf", Size: 222, ModTime: now},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{res[0].ID}, md5hex("db"), db.ReservedSnapshotPath, now); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{res[1].ID}, md5hex("doc"), "backups/docs/report.pdf", now); err != nil {
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
	if out.TotalBytes != 222 {
		t.Fatalf("total_bytes=%d want 222", out.TotalBytes)
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
			if rr.Code != http.StatusAccepted {
				t.Fatalf("status=%d want 202", rr.Code)
			}

			srv.restoreJobWg.Wait()

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
