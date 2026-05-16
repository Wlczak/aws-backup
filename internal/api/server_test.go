package api

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/events"
	"github.com/Wlczak/aws-backup/internal/source"
	"github.com/Wlczak/aws-backup/internal/storage"
)

type testServerConfig struct {
	Handler http.Handler
}

type testServer struct {
	URL    string
	Config *testServerConfig
	client *http.Client
}

func (ts *testServer) Client() *http.Client { return ts.client }
func (ts *testServer) Close()               {}

func newInProcServer(handler http.Handler) *testServer {
	ts := &testServer{
		URL:    "http://inproc.test",
		Config: &testServerConfig{Handler: handler},
	}
	transport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if ts.Config == nil || ts.Config.Handler == nil {
				return nil, errors.New("in-proc test server handler is not configured")
			}
			clientConn, serverConn := net.Pipe()
			go func() { _ = http.Serve(&singleConnListener{conn: serverConn}, ts.Config.Handler) }()
			return clientConn, nil
		},
	}
	ts.client = &http.Client{Transport: transport}
	return ts
}

type singleConnListener struct {
	conn net.Conn
	used bool
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.used {
		return nil, net.ErrClosed
	}
	l.used = true
	return l.conn, nil
}

func (l *singleConnListener) Close() error { return nil }

func (l *singleConnListener) Addr() net.Addr { return testAddr("inproc") }

type testAddr string

func (a testAddr) Network() string { return string(a) }
func (a testAddr) String() string  { return string(a) }

func newTestServer(t *testing.T) (*testServer, Deps) {
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
		BuildEngine: func(mode engine.RunMode, scanPaths []string) (*engine.Engine, error) {
			return engine.New(engine.Options{
				DB:        d,
				Source:    src,
				Storage:   store,
				TmpDir:    filepath.Join(dir, "tmp"),
				KeyPrefix: "backups",
				ChunkSize: 2,
				ZipThresh: 100,
				Mode:      mode,
				ScanPaths: scanPaths,
				Emit:      bus.Publish,
			}), nil
		},
	}
	srv := NewServer(deps)
	ts := newInProcServer(srv.Router())
	t.Cleanup(ts.Close)
	return ts, deps
}

func newRestoreDownloadServer(t *testing.T) (*testServer, Deps, storage.Storage) {
	t.Helper()
	ts, deps := newTestServer(t)
	store := storage.NewMemStorage()
	deps.Storage = func() storage.Storage { return store }
	srv := NewServer(deps)
	ts.Config.Handler = srv.Router()
	return ts, deps, store
}

func hexMD5(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func getJSON(t *testing.T, ts *testServer, path string, into any) *http.Response {
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

// TestStatusStaleCurrentRunIDIsClean verifies that when currentRun
// points at a row that no longer exists (manual delete, test cleanup
// race), /api/status returns 200 with no current run rather than a
// 500. (#182)
func TestStatusStaleCurrentRunIDIsClean(t *testing.T) {
	_, deps := newTestServer(t)
	srv := NewServer(deps)
	srv.runMu.Lock()
	srv.currentRun = 99999 // no such row
	srv.runMu.Unlock()

	ts := newInProcServer(srv.Router())
	t.Cleanup(ts.Close)

	resp, err := ts.Client().Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	var got statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Current != nil {
		t.Errorf("current=%+v want nil (stale id should look idle)", got.Current)
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

func TestSettingsPutInvokesApplySettings(t *testing.T) {
	ts, deps := newTestServer(t)

	var gotPrev, gotNext config.Config
	var applied bool
	srv := &Server{deps: Deps{
		DB:          deps.DB,
		Bus:         deps.Bus,
		Config:      deps.Config,
		ConfigPath:  deps.ConfigPath,
		BuildEngine: deps.BuildEngine,
		ApplySettings: func(prev, next config.Config) error {
			gotPrev, gotNext, applied = prev, next, true
			return nil
		},
	}}
	ts.Config.Handler = srv.Router()

	body := *deps.Config
	body.S3.Bucket = "new-bucket"
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT /api/settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		d, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, d)
	}
	if !applied {
		t.Fatal("ApplySettings was not called")
	}
	if gotPrev.S3.Bucket == gotNext.S3.Bucket {
		t.Errorf("prev/next look identical: prev=%q next=%q", gotPrev.S3.Bucket, gotNext.S3.Bucket)
	}
	if gotNext.S3.Bucket != "new-bucket" {
		t.Errorf("next.bucket=%q want new-bucket", gotNext.S3.Bucket)
	}
	if deps.Config.S3.Bucket != "new-bucket" {
		t.Errorf("live config not updated: %q", deps.Config.S3.Bucket)
	}
}

func TestSettingsPutApplyErrorRollsBack(t *testing.T) {
	ts, deps := newTestServer(t)
	origBucket := deps.Config.S3.Bucket

	srv := &Server{deps: Deps{
		DB:          deps.DB,
		Bus:         deps.Bus,
		Config:      deps.Config,
		ConfigPath:  deps.ConfigPath,
		BuildEngine: deps.BuildEngine,
		ApplySettings: func(prev, next config.Config) error {
			return fmt.Errorf("synthetic hot-swap failure")
		},
	}}
	ts.Config.Handler = srv.Router()

	body := *deps.Config
	body.S3.Bucket = "should-not-stick"
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT /api/settings: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("status=%d want 500", resp.StatusCode)
	}
	if deps.Config.S3.Bucket != origBucket {
		t.Errorf("live config changed despite apply failure: got %q", deps.Config.S3.Bucket)
	}
}

// TestSettingsConfigSharedMutex verifies that the optional Deps.ConfigMu
// is honoured by both snapshotConfig and updateConfig — when set, both
// operations should serialise on the supplied mutex so cmd-side and
// api-side writers can't race on the shared *config.Config struct
// (#153). Hammered concurrently with -race.
func TestSettingsConfigSharedMutex(t *testing.T) {
	cfg := config.Default()
	cfg.Source.LocalDir.Root = t.TempDir()
	var mu sync.RWMutex
	srv := &Server{deps: Deps{
		Config:   &cfg,
		ConfigMu: &mu,
	}}
	if got := srv.cfgMutex(); got != &mu {
		t.Fatalf("cfgMutex did not return shared mutex")
	}
	const N = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			c := config.Default()
			c.S3.Bucket = "b" + strconv.Itoa(i)
			srv.updateConfig(c)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			_, _ = srv.snapshotConfig()
		}
	}()
	wg.Wait()
}

// TestSettingsPutDuringRunDefers verifies that a PUT during an in-flight
// run no longer 409s — the merged config is persisted to disk and stashed
// as pendingConfig, ApplySettings is NOT called yet, GET surfaces
// pending_apply=true, and once the run "finishes" (we simulate by
// invoking the same flow the post-run goroutine uses) ApplySettings is
// called with (live, pending) and live state catches up.
func TestSettingsPutDuringRunDefers(t *testing.T) {
	ts, deps := newTestServer(t)

	var applyCalls int
	var lastPrev, lastNext config.Config
	srv := &Server{deps: Deps{
		DB:          deps.DB,
		Bus:         deps.Bus,
		Config:      deps.Config,
		ConfigPath:  deps.ConfigPath,
		BuildEngine: deps.BuildEngine,
		ApplySettings: func(prev, next config.Config) error {
			applyCalls++
			lastPrev, lastNext = prev, next
			return nil
		},
	}}
	// Pretend a run is in flight.
	srv.currentRun = 99
	srv.currentRunCancel = func() {}
	ts.Config.Handler = srv.Router()

	body := *deps.Config
	body.S3.Bucket = "queued-bucket"
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		d, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s want 200", resp.StatusCode, d)
	}
	var saved settingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	if !saved.PendingApply {
		t.Error("response.pending_apply=false, want true")
	}
	if saved.S3.Bucket != "queued-bucket" {
		t.Errorf("response.s3.bucket=%q want queued-bucket", saved.S3.Bucket)
	}
	if applyCalls != 0 {
		t.Errorf("ApplySettings called %d times during run; want 0", applyCalls)
	}
	if deps.Config.S3.Bucket == "queued-bucket" {
		t.Error("live config was mutated mid-run")
	}
	if srv.pendingConfig == nil || srv.pendingConfig.S3.Bucket != "queued-bucket" {
		t.Errorf("pendingConfig=%+v want queued-bucket", srv.pendingConfig)
	}

	// GET surfaces the pending values + flag.
	var got settingsResponse
	getJSON(t, ts, "/api/settings", &got)
	if !got.PendingApply || got.S3.Bucket != "queued-bucket" {
		t.Errorf("GET surface: pending=%v bucket=%q", got.PendingApply, got.S3.Bucket)
	}

	// Simulate run completion: the post-run goroutine clears currentRun,
	// drains pending, and calls applyPendingSettings.
	srv.runMu.Lock()
	srv.currentRun = 0
	srv.currentRunCancel = nil
	pending := srv.pendingConfig
	srv.pendingConfig = nil
	srv.runMu.Unlock()
	srv.applyPendingSettings(pending, nil)

	if applyCalls != 1 {
		t.Errorf("ApplySettings calls=%d after run end; want 1", applyCalls)
	}
	if lastPrev.S3.Bucket == lastNext.S3.Bucket {
		t.Errorf("prev/next look identical post-apply: prev=%q next=%q",
			lastPrev.S3.Bucket, lastNext.S3.Bucket)
	}
	if lastNext.S3.Bucket != "queued-bucket" {
		t.Errorf("apply received next.bucket=%q want queued-bucket", lastNext.S3.Bucket)
	}
	if deps.Config.S3.Bucket != "queued-bucket" {
		t.Errorf("live config not updated post-apply: %q", deps.Config.S3.Bucket)
	}

	// GET now reflects live state with no pending flag.
	var after settingsResponse
	getJSON(t, ts, "/api/settings", &after)
	if after.PendingApply {
		t.Error("pending_apply still true after apply")
	}
}

// TestSettingsPutDuringRunComposesPending verifies that successive PUTs
// during one run compose against the previous pending config so a
// redacted-secret echo doesn't blank the credentials the operator just
// queued.
func TestSettingsPutDuringRunComposesPending(t *testing.T) {
	ts, deps := newTestServer(t)
	deps.Config.S3.SecretAccessKey = "live-secret"

	srv := &Server{deps: Deps{
		DB:            deps.DB,
		Bus:           deps.Bus,
		Config:        deps.Config,
		ConfigPath:    deps.ConfigPath,
		BuildEngine:   deps.BuildEngine,
		ApplySettings: func(prev, next config.Config) error { return nil },
	}}
	srv.currentRun = 99
	srv.currentRunCancel = func() {}
	ts.Config.Handler = srv.Router()

	// First PUT: change the secret to a real new value.
	first := *deps.Config
	first.S3.SecretAccessKey = "new-secret"
	first.S3.Bucket = "first-bucket"
	b, _ := json.Marshal(first)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("first PUT /api/settings: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("first PUT status=%d want 200", resp.StatusCode)
	}

	// Second PUT: GET-then-PUT pattern with the redacted secret echoed
	// back. Should preserve "new-secret" (from pendingConfig), NOT fall
	// back to "live-secret".
	second := *deps.Config
	second.S3.SecretAccessKey = config.RedactedMarker
	second.S3.Bucket = "second-bucket"
	b, _ = json.Marshal(second)
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/settings", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatalf("second PUT /api/settings: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("second PUT status=%d want 200", resp.StatusCode)
	}

	if srv.pendingConfig == nil {
		t.Fatal("pendingConfig nil after second PUT")
	}
	if srv.pendingConfig.S3.SecretAccessKey != "new-secret" {
		t.Errorf("secret=%q want new-secret (composed against pending)",
			srv.pendingConfig.S3.SecretAccessKey)
	}
	if srv.pendingConfig.S3.Bucket != "second-bucket" {
		t.Errorf("bucket=%q want second-bucket", srv.pendingConfig.S3.Bucket)
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
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT /api/settings: %v", err)
	}
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

func TestFilesListAll(t *testing.T) {
	ts, deps := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Insert more rows than the default page size to prove all=true
	// really bypasses pagination.
	for i := 0; i < 75; i++ {
		_, _ = deps.DB.UpsertFile(ctx, fmt.Sprintf("dir/f%02d.txt", i), 10, now, now)
	}

	var got filesListResponse
	getJSON(t, ts, "/api/files?all=true", &got)
	if got.Total != 75 || len(got.Files) != 75 {
		t.Errorf("total=%d len=%d want 75/75", got.Total, len(got.Files))
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

// TestStopRun verifies the graceful-stop endpoint flips the server's
// stop flag (which the engine polls between files) and returns 202.
// Cancel-vs-stop semantic separation: /cancel still kills mid-stream;
// /stop only requests an after-current exit. (#124)
func TestStopRun(t *testing.T) {
	ts, deps := newTestServer(t)

	// Pretend a run is in flight under id 42.
	srv := &Server{deps: deps, currentRun: 42, currentRunCancel: func() {}}
	ts.Config.Handler = srv.Router()

	if srv.IsStopRequested() {
		t.Fatal("stop flag should be clear before /stop")
	}

	// Wrong id: 404, flag stays clear.
	resp, err := ts.Client().Post(ts.URL+"/api/runs/41/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("wrong-id status=%d want 404", resp.StatusCode)
	}
	if srv.IsStopRequested() {
		t.Fatal("wrong-id call must not flip the stop flag")
	}

	// Right id: 202 + flag set.
	resp, err = ts.Client().Post(ts.URL+"/api/runs/42/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status=%d want 202", resp.StatusCode)
	}
	if !srv.IsStopRequested() {
		t.Error("stop flag should be set after /stop")
	}

	// /continue clears the flag.
	resp, err = ts.Client().Post(ts.URL+"/api/runs/42/continue", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("/continue status=%d want 202", resp.StatusCode)
	}
	if srv.IsStopRequested() {
		t.Error("stop flag should be clear after /continue")
	}

	// Idle server: 404.
	idle := &Server{deps: deps}
	ts.Config.Handler = idle.Router()
	resp, err = ts.Client().Post(ts.URL+"/api/runs/1/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("idle status=%d want 404", resp.StatusCode)
	}
}

// TestMaybeSyncDBToS3 covers the post-run branching that decides whether
// to upload the index DB to S3 after a run ends, based on how the run
// terminated. (#128)
func TestMaybeSyncDBToS3(t *testing.T) {
	type call struct {
		runID  int64
		reason string
	}

	cases := []struct {
		name      string
		mode      engine.RunMode
		runErr    error
		stopReq   bool
		cancelReq bool
		want      *call // nil = expect no sync
	}{
		{name: "stop triggers sync", mode: engine.RunModeFull, runErr: nil, stopReq: true, want: &call{runID: 7, reason: "stop"}},
		{name: "force-cancel skips sync", mode: engine.RunModeFull, runErr: context.Canceled, cancelReq: true, want: nil},
		{name: "completion triggers sync", mode: engine.RunModeFull, runErr: nil, want: &call{runID: 7, reason: "complete"}},
		{name: "upload-mode completion triggers sync", mode: engine.RunModeUpload, runErr: nil, want: &call{runID: 7, reason: "complete"}},
		{name: "scan-mode completion skips sync", mode: engine.RunModeScan, runErr: nil, want: nil},
		{name: "scan-mode stop skips sync", mode: engine.RunModeScan, runErr: nil, stopReq: true, want: nil},
		{name: "engine failure skips sync", mode: engine.RunModeFull, runErr: errors.New("boom"), want: nil},
		{name: "shutdown-cancel skips sync", mode: engine.RunModeFull, runErr: context.Canceled, cancelReq: false, want: nil},
		{name: "stop with non-nil runErr skips", mode: engine.RunModeFull, runErr: errors.New("boom"), stopReq: true, want: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got *call
			fake := func(ctx context.Context, runID int64, reason string) error {
				got = &call{runID: runID, reason: reason}
				return nil
			}
			s := NewServer(Deps{SyncDBToS3: fake})
			s.maybeSyncDBToS3(s.deps.SyncDBToS3, s.deps.Logger, 7, tc.mode, tc.runErr, tc.stopReq, tc.cancelReq)

			if tc.want == nil {
				if got != nil {
					t.Fatalf("want no sync, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("want sync called with %+v, got none", tc.want)
			}
			if *got != *tc.want {
				t.Errorf("call=%+v want %+v", got, tc.want)
			}
		})
	}
}

// TestMaybeSyncDBToS3ShutdownAbortsInFlight covers the SIGINT-during-sync
// case: closing shutdownCh while syncDBToS3 is running must cancel its
// context so app.close() doesn't race a tear-down. (#128)
func TestMaybeSyncDBToS3ShutdownAbortsInFlight(t *testing.T) {
	syncStarted := make(chan struct{})
	syncCtx := make(chan context.Context, 1)
	fake := func(ctx context.Context, runID int64, reason string) error {
		close(syncStarted)
		syncCtx <- ctx
		<-ctx.Done()
		return ctx.Err()
	}
	s := NewServer(Deps{SyncDBToS3: fake})

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.maybeSyncDBToS3(s.deps.SyncDBToS3, s.deps.Logger, 1, engine.RunModeFull, nil, true /* stopReq */, false)
	}()

	select {
	case <-syncStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("sync never started")
	}
	// Trigger shutdown — the sync ctx should be cancelled promptly.
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("maybeSyncDBToS3 did not return after Shutdown")
	}
	ctx := <-syncCtx
	if ctx.Err() == nil {
		t.Errorf("sync ctx not cancelled after Shutdown")
	}
}

// TestMaybeSyncDBToS3SkipsAfterShutdown covers the case where shutdown
// fires BEFORE the post-run goroutine reaches the sync — the upload must
// not be initiated at all (per #128: no DB sync on service shutdown).
func TestMaybeSyncDBToS3SkipsAfterShutdown(t *testing.T) {
	called := false
	fake := func(ctx context.Context, runID int64, reason string) error {
		called = true
		return nil
	}
	s := NewServer(Deps{SyncDBToS3: fake})
	_ = s.Shutdown(context.Background())
	s.maybeSyncDBToS3(s.deps.SyncDBToS3, s.deps.Logger, 1, engine.RunModeFull, nil, false, false /* completion */)
	if called {
		t.Error("sync was called even though shutdown had already started")
	}
}

// TestCancelRunSetsCancelReq verifies /api/runs/:id/cancel sets the flag
// the post-run goroutine reads to distinguish user-cancel from
// service-shutdown-cancel. (#128)
func TestCancelRunSetsCancelReq(t *testing.T) {
	ts, deps := newTestServer(t)
	cancelCalled := false
	srv := &Server{
		deps:             deps,
		currentRun:       42,
		currentRunCancel: func() { cancelCalled = true },
		shutdownCh:       make(chan struct{}),
	}
	ts.Config.Handler = srv.Router()

	resp, err := ts.Client().Post(ts.URL+"/api/runs/42/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status=%d want 202", resp.StatusCode)
	}
	if !cancelCalled {
		t.Error("cancel func not invoked")
	}
	if !srv.currentRunCancelReq.Load() {
		t.Error("currentRunCancelReq should be set after /cancel")
	}
}

// TestStorageHotSwap is the regression test for #131: changing the
// underlying storage handle (via Deps.Storage callback) must take effect
// on the next request without a server restart. Before the fix, Deps
// captured storage by value, so /api/sync etc. kept calling the old
// endpoint after a settings hot-swap.
func TestStorageHotSwap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	now := time.Now().UTC()
	r, err := d.UpsertFile(ctx, "z.txt", 1, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploaded(ctx, r.ID, "md5", "z.txt", now); err != nil {
		t.Fatal(err)
	}

	old := storage.NewMemStorage()
	current := storage.NewMemStorage()
	// Seed only `current` with z.txt — `old` is empty. After the swap,
	// /api/sync should see z.txt and NOT mark it pending. Before the
	// swap it would mark it missing.
	if _, err := current.PutStandard(ctx, "z.txt", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	var live storage.Storage = old
	srv := NewServer(Deps{
		DB:      d,
		Bus:     events.NewBus(16),
		Config:  &cfg,
		Storage: func() storage.Storage { return live },
	})
	ts := newInProcServer(srv.Router())
	t.Cleanup(ts.Close)

	// Hot-swap the storage handle that the getter returns.
	live = current

	resp, err := ts.Client().Post(ts.URL+"/api/sync", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got syncResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	if got.MissingIndividual != 0 {
		t.Errorf("missing_individual=%d want 0 (hot-swapped storage has the key)", got.MissingIndividual)
	}
	if got.KeysInS3 != 1 {
		t.Errorf("keys_in_s3=%d want 1 (post-swap storage)", got.KeysInS3)
	}
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
	ts, _ := newTestServer(t)

	var res testResult
	getJSON(t, ts, "/api/smb/test", &res)
	if !res.OK {
		t.Errorf("source test: ok=false msg=%s", res.Message)
	}
}

// TestTestEndpointS3 hits the live HeadBucket round-trip handleTestStorage
// performs. Requires the docker-compose MinIO at localhost:9000; skipped
// when unreachable so CI (no MinIO sidecar) stays green. Mirrors the
// probe in s3_integration_test.go.
func TestTestEndpointS3(t *testing.T) {
	conn, err := net.DialTimeout("tcp", "localhost:9000", 300*time.Millisecond)
	if err != nil {
		t.Skipf("skipping: MinIO not reachable at localhost:9000 (%v)", err)
	}
	conn.Close()

	ts, deps := newTestServer(t)
	deps.Config.S3.Endpoint = "http://localhost:9000"

	var res testResult
	getJSON(t, ts, "/api/s3/test", &res)
	if !res.OK {
		t.Errorf("storage test: ok=false msg=%s", res.Message)
	}
}

func TestRestoreEstimate(t *testing.T) {
	ts, deps := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a, _ := deps.DB.UpsertFile(ctx, "photos/a.jpg", 1024*1024*1024, now, now) // 1 GB
	b, _ := deps.DB.UpsertFile(ctx, "photos/b.jpg", 1024*1024*1024, now, now)
	c, _ := deps.DB.UpsertFile(ctx, "docs/x.pdf", 500, now, now)
	// Only uploaded/zipped files are restorable. Mark the photos uploaded
	// (one individual, one zipped) and leave docs/x.pdf as pending; it
	// must NOT contribute to the estimate.
	_ = deps.DB.MarkUploaded(ctx, a.ID, "m", "k1", now)
	_ = deps.DB.SetZipName(ctx, []int64{b.ID}, "photos/photos_1.zip")
	_ = c // unused; left as pending

	body := strings.NewReader(`{"paths":["photos","unknown/dir"],"tier":"standard","days":30}`)
	resp, err := ts.Client().Post(ts.URL+"/api/restore/estimate", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
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
	if got.StorageFeeUSD <= 0 {
		t.Errorf("storage_fee_usd not positive: %v", got.StorageFeeUSD)
	}
	if got.TotalFeeUSD <= 0 {
		t.Errorf("total fee not positive: %v", got.TotalFeeUSD)
	}
	if len(got.UnknownPaths) != 1 || got.UnknownPaths[0] != "unknown/dir" {
		t.Errorf("unknown_paths=%+v", got.UnknownPaths)
	}
}

func TestRestoreDownloadOK(t *testing.T) {
	ts, deps, store := newRestoreDownloadServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r, err := deps.DB.UpsertFile(ctx, "notes.txt", 5, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.DB.MarkUploaded(ctx, r.ID, hexMD5("hello"), "backups/notes.txt", now); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.DB.MarkRestored(ctx, "backups/notes.txt", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, "backups/notes.txt", strings.NewReader("hello"), int64(len("hello"))); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "restore-out")
	body := fmt.Sprintf(`{"paths":["notes.txt"],"target_dir":%q,"verify_checksum":true}`, target)
	resp, err := ts.Client().Post(ts.URL+"/api/restore/download", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}

	deadline := time.Now().Add(2 * time.Second)
	var got struct {
		RestoreDownloadCurrent *restoreDownloadSummary `json:"restore_download_current"`
		RestoreDownloadLast    *restoreDownloadSummary `json:"restore_download_last"`
	}
	for time.Now().Before(deadline) {
		statusResp, err := ts.Client().Get(ts.URL + "/api/status")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(statusResp.Body).Decode(&got); err != nil {
			statusResp.Body.Close()
			t.Fatalf("decode status: %v", err)
		}
		statusResp.Body.Close()
		if got.RestoreDownloadLast != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.RestoreDownloadLast == nil {
		t.Fatal("restore download did not finish")
	}
	if got.RestoreDownloadLast.FilesWritten != 1 {
		t.Fatalf("files_written=%d want 1", got.RestoreDownloadLast.FilesWritten)
	}
	if got.RestoreDownloadLast.BytesWritten != int64(len("hello")) {
		t.Fatalf("bytes_written=%d want %d", got.RestoreDownloadLast.BytesWritten, len("hello"))
	}
	data, err := os.ReadFile(filepath.Join(target, "notes.txt"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("restored file = %q want %q", data, "hello")
	}
}

func TestRestoreDownloadEstimate(t *testing.T) {
	ts, deps := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	res, err := deps.DB.UpsertFileBatch(ctx, []db.BatchEntry{
		{Path: "photos/a.jpg", Size: 100, ModTime: now},
		{Path: "photos/b.jpg", Size: 200, ModTime: now},
		{Path: "docs/readme.md", Size: 300, ModTime: now},
		{Path: "docs/idle.txt", Size: 400, ModTime: now},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{
		"photos/a.jpg":   res[0].ID,
		"photos/b.jpg":   res[1].ID,
		"docs/readme.md": res[2].ID,
		"docs/idle.txt":  res[3].ID,
	}
	if err := deps.DB.SetZipName(ctx, []int64{ids["photos/a.jpg"], ids["photos/b.jpg"]}, "photos/photos_1.zip"); err != nil {
		t.Fatal(err)
	}
	if err := deps.DB.MarkUploadedBatch(ctx, []int64{ids["photos/a.jpg"]}, md5hex("aaa"), "backups/photos/photos_1.zip", now); err != nil {
		t.Fatal(err)
	}
	if err := deps.DB.MarkUploadedBatch(ctx, []int64{ids["photos/b.jpg"]}, md5hex("bbbb"), "backups/photos/photos_1.zip", now); err != nil {
		t.Fatal(err)
	}
	if err := deps.DB.MarkUploaded(ctx, ids["docs/readme.md"], md5hex("docs!"), "backups/docs/readme.md", now); err != nil {
		t.Fatal(err)
	}
	if err := deps.DB.MarkUploaded(ctx, ids["docs/idle.txt"], md5hex("idle"), "backups/docs/idle.txt", now); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.DB.MarkRestored(ctx, "backups/photos/photos_1.zip", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.DB.MarkRestoreInProgress(ctx, "backups/docs/readme.md"); err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"paths":["photos","docs","unknown/dir"]}`)
	resp, err := ts.Client().Post(ts.URL+"/api/restore/download/estimate", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var got restoreDownloadEstimateResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ObjectCount != 1 {
		t.Fatalf("object_count=%d want 1", got.ObjectCount)
	}
	if got.TotalBytes != 300 {
		t.Fatalf("total_bytes=%d want 300", got.TotalBytes)
	}
	if got.RestoredCount != 2 {
		t.Fatalf("restored_count=%d want 2", got.RestoredCount)
	}
	if got.InProgressCount != 1 {
		t.Fatalf("in_progress_count=%d want 1", got.InProgressCount)
	}
	if got.NotRestoringCount != 1 {
		t.Fatalf("not_restoring_count=%d want 1", got.NotRestoringCount)
	}
	if len(got.UnknownPaths) != 1 || got.UnknownPaths[0] != "unknown/dir" {
		t.Fatalf("unknown_paths=%v", got.UnknownPaths)
	}
}

func TestDownloadStatusCarriesCostEstimate(t *testing.T) {
	srv := &Server{}
	srv.currentDownload = &downloadSummary{
		ID:          1,
		StartedAt:   time.Now().UTC(),
		Status:      "running",
		Phase:       "scan",
		DownloadDir: t.TempDir(),
	}

	srv.applyDownloadEvent(engine.Event{
		Type: engine.EventDownloadMirrorScanComplete,
		Data: map[string]any{
			"scanned":      4,
			"present":      2,
			"missing":      2,
			"total":        2,
			"total_bytes":  int64(101 * 1024 * 1024 * 1024),
			"object_count": int64(3),
		},
	})

	got := srv.currentDownload
	if got == nil {
		t.Fatal("currentDownload cleared unexpectedly")
	}
	if got.ObjectCount != 3 {
		t.Fatalf("object_count=%d want 3", got.ObjectCount)
	}
	if got.RequestFeeUSD <= 0 || got.EgressFeeUSD <= 0 || got.TotalFeeUSD <= 0 {
		t.Fatalf("expected positive fees: %+v", got)
	}
}

func TestRestoreDownloadEstimateRejectsEmptyPaths(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := ts.Client().Post(ts.URL+"/api/restore/download/estimate", "application/json",
		strings.NewReader(`{"paths":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestRestoreDownloadRejectsRelativeTarget(t *testing.T) {
	ts, deps, _ := newRestoreDownloadServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r, err := deps.DB.UpsertFile(ctx, "notes.txt", 5, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.DB.MarkUploaded(ctx, r.ID, hexMD5("hello"), "backups/notes.txt", now); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.DB.MarkRestoreInProgress(ctx, "backups/notes.txt"); err != nil {
		t.Fatal(err)
	}

	resp, err := ts.Client().Post(ts.URL+"/api/restore/download", "application/json",
		strings.NewReader(`{"paths":["notes.txt"],"target_dir":"relative/path"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestRestoreDownloadRejectsEmptyPaths(t *testing.T) {
	ts, _, _ := newRestoreDownloadServer(t)
	resp, err := ts.Client().Post(ts.URL+"/api/restore/download", "application/json",
		strings.NewReader(`{"paths":[],"target_dir":"/tmp/restore"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestRestoreDownloadRejectsWhileRunActive(t *testing.T) {
	ts, deps, _ := newRestoreDownloadServer(t)
	srv := NewServer(deps)
	srv.runMu.Lock()
	srv.currentRun = 1
	srv.runMu.Unlock()
	ts.Config.Handler = srv.Router()

	resp, err := ts.Client().Post(ts.URL+"/api/restore/download", "application/json",
		strings.NewReader(`{"paths":["notes.txt"],"target_dir":"/tmp/restore"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d want 409", resp.StatusCode)
	}
}

func TestRetryFile(t *testing.T) {
	ts, deps := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r, _ := deps.DB.UpsertFile(ctx, "a.txt", 10, now, now)
	_ = deps.DB.MarkFailed(ctx, r.ID)

	resp, err := ts.Client().Post(fmt.Sprintf("%s/api/files/%d/retry", ts.URL, r.ID), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		d, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, d)
	}
	resp.Body.Close()

	files, _, _ := deps.DB.ListFiles(ctx, db.FilesFilter{})
	if files[0].Status != db.StatusPending {
		t.Errorf("status=%q want pending", files[0].Status)
	}
}

func TestRetryFilesBulkAllFailed(t *testing.T) {
	ts, deps := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r1, _ := deps.DB.UpsertFile(ctx, "a.txt", 1, now, now)
	r2, _ := deps.DB.UpsertFile(ctx, "b.txt", 1, now, now)
	_ = deps.DB.MarkFailed(ctx, r1.ID)
	_ = deps.DB.MarkFailed(ctx, r2.ID)

	resp, err := ts.Client().Post(ts.URL+"/api/files/retry", "application/json",
		strings.NewReader(`{"all_failed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var body affectedResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body.Affected != 2 {
		t.Errorf("affected=%d want 2", body.Affected)
	}
}

func TestDeleteFile(t *testing.T) {
	ts, deps := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r, _ := deps.DB.UpsertFile(ctx, "a.txt", 10, now, now)

	req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/files/%d", ts.URL, r.ID), nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		d, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, d)
	}
	resp.Body.Close()

	_, total, _ := deps.DB.ListFiles(ctx, db.FilesFilter{})
	if total != 0 {
		t.Errorf("total=%d want 0", total)
	}
}

func TestDeleteFilesBulk(t *testing.T) {
	ts, deps := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r1, _ := deps.DB.UpsertFile(ctx, "a.txt", 1, now, now)
	r2, _ := deps.DB.UpsertFile(ctx, "b.txt", 1, now, now)
	_, _ = deps.DB.UpsertFile(ctx, "c.txt", 1, now, now)

	body := fmt.Sprintf(`{"ids":[%d,%d]}`, r1.ID, r2.ID)
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/files", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var res affectedResponse
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if res.Affected != 2 {
		t.Errorf("affected=%d", res.Affected)
	}
}

func TestDeleteRunLogs(t *testing.T) {
	ts, deps := newTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	r1, _ := deps.DB.CreateRun(ctx, now)
	r2, _ := deps.DB.CreateRun(ctx, now.Add(time.Minute))
	_ = deps.DB.AppendLog(ctx, r1, db.LogInfo, "hello", now)
	_ = deps.DB.AppendLog(ctx, r2, db.LogError, "boom", now.Add(time.Minute))

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/run-logs", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		d, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, d)
	}
	var res affectedResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if res.Affected != 2 {
		t.Fatalf("affected=%d want 2", res.Affected)
	}
	if _, total, err := deps.DB.ListLogs(ctx, r1, 1, 10); err != nil || total != 0 {
		t.Fatalf("run1 logs=%d err=%v", total, err)
	}
	if _, total, err := deps.DB.ListLogs(ctx, r2, 1, 10); err != nil || total != 0 {
		t.Fatalf("run2 logs=%d err=%v", total, err)
	}
}

func TestRestoreTriggerWithoutStorage(t *testing.T) {
	// newTestServer doesn't wire Deps.Storage, so the handler should
	// refuse the request rather than attempt a download.
	ts, _ := newTestServer(t)
	resp, err := ts.Client().Post(ts.URL+"/api/restore/trigger", "application/json",
		strings.NewReader(`{"paths":["photos"],"target_dir":"/tmp/restore"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRestoreSyncStatus_NotConfigured(t *testing.T) {
	// newTestServer doesn't wire Deps.SyncRestoreStatus, so the handler
	// should respond 503 with a friendly message that the UI can render.
	ts, _ := newTestServer(t)
	resp, err := ts.Client().Post(ts.URL+"/api/restore/sync-status", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", resp.StatusCode)
	}
}

func TestRestoreSyncStatus_OK(t *testing.T) {
	ts, deps := newTestServer(t)
	calls := 0
	deps.SyncRestoreStatus = func(_ context.Context) (int, error) {
		calls++
		return 7, nil
	}
	srv := NewServer(deps)
	ts.Config.Handler = srv.Router()

	resp, err := ts.Client().Post(ts.URL+"/api/restore/sync-status", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got struct{ Processed int }
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Processed != 7 {
		t.Errorf("processed=%d want 7", got.Processed)
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1", calls)
	}
}

func TestRestoreSyncStatus_DrainError(t *testing.T) {
	ts, deps := newTestServer(t)
	deps.SyncRestoreStatus = func(_ context.Context) (int, error) {
		return 0, errors.New("boom")
	}
	srv := NewServer(deps)
	ts.Config.Handler = srv.Router()

	resp, err := ts.Client().Post(ts.URL+"/api/restore/sync-status", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", resp.StatusCode)
	}
}
