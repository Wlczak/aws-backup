package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/events"
	"github.com/Wlczak/aws-backup/internal/source"
	"github.com/Wlczak/aws-backup/internal/storage"
)

func newTestServer(t *testing.T) (*httptest.Server, Deps) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	cfg := config.Default()
	cfg.Source.LocalDir.Root = dir
	cfgPath := filepath.Join(dir, "config.json")
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	bus := events.NewBus(16)

	src, err := source.NewLocalDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })

	store := storage.NewMemStorage()

	deps := Deps{
		DB:         d,
		Bus:        bus,
		Config:     &cfg,
		ConfigPath: cfgPath,
		BuildEngine: func() (*engine.Engine, error) {
			return engine.New(engine.Options{
				DB:        d,
				Source:    src,
				Storage:   store,
				TmpDir:    filepath.Join(dir, "tmp"),
				KeyPrefix: "backups",
				ChunkSize: 2,
				ZipThresh: 100,
				Emit:      bus.Publish,
			}), nil
		},
	}
	srv := NewServer(deps)
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return ts, deps
}

func getJSON(t *testing.T, ts *httptest.Server, path string, into any) *http.Response {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	if into != nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err := json.Unmarshal(b, into); err != nil {
			t.Fatalf("unmarshal %s: %v (body=%s)", path, err, b)
		}
	}
	return resp
}

func TestStatusEmpty(t *testing.T) {
	ts, _ := newTestServer(t)
	var got statusResponse
	getJSON(t, ts, "/api/status", &got)
	if got.Current != nil {
		t.Errorf("current=%+v want nil", got.Current)
	}
	if got.Last != nil {
		t.Errorf("last=%+v want nil", got.Last)
	}
}

func TestSettingsRedaction(t *testing.T) {
	ts, deps := newTestServer(t)
	deps.Config.S3.AccessKeyID = "AKIA12345"
	deps.Config.S3.SecretAccessKey = "topsecret"

	var got config.Config
	getJSON(t, ts, "/api/settings", &got)
	if got.S3.AccessKeyID != config.RedactedMarker || got.S3.SecretAccessKey != config.RedactedMarker {
		t.Errorf("credentials not redacted: %+v", got.S3)
	}
}

func TestSettingsPutPreservesRedacted(t *testing.T) {
	ts, deps := newTestServer(t)
	deps.Config.S3.SecretAccessKey = "original"

	body := config.Default()
	body.Source.LocalDir.Root = deps.Config.Source.LocalDir.Root
	body.S3.SecretAccessKey = config.RedactedMarker // client echoed "***"
	body.S3.Bucket = "renamed-bucket"
	b, _ := json.Marshal(body)

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		d, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, d)
	}
	resp.Body.Close()

	if deps.Config.S3.SecretAccessKey != "original" {
		t.Errorf("secret was overwritten: %q", deps.Config.S3.SecretAccessKey)
	}
	if deps.Config.S3.Bucket != "renamed-bucket" {
		t.Errorf("bucket not updated: %q", deps.Config.S3.Bucket)
	}
}

func TestSettingsPutInvalidRejected(t *testing.T) {
	ts, deps := newTestServer(t)
	bad := config.Default()
	bad.Source.LocalDir.Root = deps.Config.Source.LocalDir.Root
	bad.Backup.Schedule = "not a cron"
	b, _ := json.Marshal(bad)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := ts.Client().Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestFilesList(t *testing.T) {
	ts, deps := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, p := range []string{"a.txt", "b.txt", "c.txt"} {
		_, _ = deps.DB.UpsertFile(ctx, p, 10, now, now)
	}

	var got filesListResponse
	getJSON(t, ts, "/api/files?page=1&limit=2", &got)
	if got.Total != 3 || len(got.Files) != 2 {
		t.Errorf("total=%d len=%d want 3/2", got.Total, len(got.Files))
	}

	var stats fileStatsResponse
	getJSON(t, ts, "/api/files/stats", &stats)
	if stats.TotalCount != 3 {
		t.Errorf("stats.TotalCount=%d want 3", stats.TotalCount)
	}
}

func TestRunsList(t *testing.T) {
	ts, deps := newTestServer(t)
	ctx := context.Background()
	id, _ := deps.DB.CreateRun(ctx, time.Now().UTC())
	_ = deps.DB.FinishRun(ctx, id, db.RunCompleted, "", time.Now().UTC().Add(time.Second))

	var got runsListResponse
	getJSON(t, ts, "/api/runs", &got)
	if got.Total != 1 || len(got.Runs) != 1 {
		t.Errorf("want 1/1, got %d/%d", got.Total, len(got.Runs))
	}

	var detail runDetailResponse
	getJSON(t, ts, fmt.Sprintf("/api/runs/%d", id), &detail)
	if detail.Run.ID != id {
		t.Errorf("id=%d want %d", detail.Run.ID, id)
	}
}

func TestTriggerRun(t *testing.T) {
	ts, deps := newTestServer(t)

	resp, err := ts.Client().Post(ts.URL+"/api/runs", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		d, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, d)
	}
	var tr triggerRunResponse
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	resp.Body.Close()
	if tr.RunID == 0 {
		t.Fatal("expected non-zero run id")
	}

	// Wait for run to finish (no files -> completes quickly).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := deps.DB.GetRun(context.Background(), tr.RunID)
		if r.Status == db.RunCompleted {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %d did not complete in time", tr.RunID)
}

func TestTriggerRunConflict(t *testing.T) {
	ts, deps := newTestServer(t)

	// Simulate in-flight run by poking server internal state.
	srv := &Server{deps: deps, currentRun: 99}
	ts.Config.Handler = srv.Router() // swap handler

	resp, err := ts.Client().Post(ts.URL+"/api/runs", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status=%d want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestTestEndpoints(t *testing.T) {
	ts, deps := newTestServer(t)
	deps.Config.Source.LocalDir.Root = deps.Config.Source.LocalDir.Root // existing dir

	var res testResult
	getJSON(t, ts, "/api/smb/test", &res)
	if !res.OK {
		t.Errorf("source test: ok=false msg=%s", res.Message)
	}

	deps.Config.S3.Endpoint = "http://localhost:9000"
	getJSON(t, ts, "/api/s3/test", &res)
	if !res.OK {
		t.Errorf("storage test: ok=false msg=%s", res.Message)
	}
}

func TestRestoreEstimate(t *testing.T) {
	ts, deps := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_, _ = deps.DB.UpsertFile(ctx, "photos/a.jpg", 1024*1024*1024, now, now) // 1 GB
	_, _ = deps.DB.UpsertFile(ctx, "photos/b.jpg", 1024*1024*1024, now, now)
	_, _ = deps.DB.UpsertFile(ctx, "docs/x.pdf", 500, now, now)

	body := strings.NewReader(`{"paths":["photos","unknown/dir"]}`)
	resp, err := ts.Client().Post(ts.URL+"/api/restore/estimate", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	var got restoreEstimateResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()

	if got.FileCount != 2 {
		t.Errorf("file_count=%d want 2", got.FileCount)
	}
	if got.TotalBytes != 2*1024*1024*1024 {
		t.Errorf("total_bytes=%d", got.TotalBytes)
	}
	if got.TotalFeeUSD <= 0 {
		t.Errorf("total fee not positive: %v", got.TotalFeeUSD)
	}
	if len(got.UnknownPaths) != 1 || got.UnknownPaths[0] != "unknown/dir" {
		t.Errorf("unknown_paths=%+v", got.UnknownPaths)
	}
}

func TestRestoreTriggerGated(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := ts.Client().Post(ts.URL+"/api/restore/trigger", "application/json",
		strings.NewReader(`{"paths":["photos"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", resp.StatusCode)
	}
	resp.Body.Close()
}
