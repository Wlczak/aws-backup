package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/restore"
	"github.com/Wlczak/aws-backup/internal/source"
	"github.com/Wlczak/aws-backup/internal/storage"
	"golang.org/x/crypto/bcrypt"
)

func TestRefreshDBFromS3_NoRemote(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "index.db")
	if err := os.WriteFile(dst, []byte("local"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store := storage.NewMemStorage()
	if err := refreshDBFromS3(context.Background(), store, "", dst); err != nil {
		t.Fatalf("err: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "local" {
		t.Errorf("local was overwritten: %q", got)
	}
}

func TestRefreshDBFromS3_LocalMissingDownloads(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "index.db")
	store := storage.NewMemStorage()
	if _, err := store.PutStandard(context.Background(), "index.db", bytes.NewReader([]byte("remote")), -1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := refreshDBFromS3(context.Background(), store, "", dst); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "remote" {
		t.Errorf("got %q want remote", got)
	}
}

func TestRefreshDBFromS3_RemoteNewerWins(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "index.db")
	if err := os.WriteFile(dst, []byte("local-old"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(dst, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	store := storage.NewMemStorage()
	if _, err := store.PutStandard(context.Background(), "index.db", bytes.NewReader([]byte("remote-new")), -1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := refreshDBFromS3(context.Background(), store, "", dst); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "remote-new" {
		t.Errorf("got %q, expected remote to win", got)
	}
}

func TestRefreshDBFromS3_LocalNewerKept(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "index.db")
	store := storage.NewMemStorage()
	// Remote first (older), then write local with current mtime (newer).
	if _, err := store.PutStandard(context.Background(), "index.db", bytes.NewReader([]byte("remote-old")), -1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Push the remote LastModified into the past so local clearly wins
	// even with the 1s skew tolerance.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(dst, []byte("local-new"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	now := time.Now().Add(time.Hour)
	if err := os.Chtimes(dst, now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := refreshDBFromS3(context.Background(), store, "", dst); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "local-new" {
		t.Errorf("got %q, expected local to win", got)
	}
}

func TestSyncDBToS3UploadsReadableSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.db")

	d, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	now := time.Now().UTC()
	r, err := d.UpsertFile(ctx, "a.txt", 1, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploaded(ctx, r.ID, "m1", "k1", now); err != nil {
		t.Fatal(err)
	}

	store := storage.NewMemStorage()
	a := &appState{
		db:     d,
		dbPath: dbPath,
		store:  store,
		cfg:    config.Config{},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := a.syncDBToS3(ctx, 7, "complete"); err != nil {
		t.Fatalf("syncDBToS3: %v", err)
	}

	raw, ok := store.GetBytes("index.db")
	if !ok {
		t.Fatal("missing uploaded index.db")
	}
	snapPath := filepath.Join(t.TempDir(), "uploaded.db")
	if err := os.WriteFile(snapPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := db.Open(ctx, snapPath)
	if err != nil {
		t.Fatalf("open uploaded snapshot: %v", err)
	}
	t.Cleanup(func() { _ = snap.Close() })
	files, total, err := snap.ListFiles(ctx, db.FilesFilter{})
	if err != nil {
		t.Fatalf("list uploaded snapshot: %v", err)
	}
	if total != 1 || len(files) != 1 || files[0].Path != "a.txt" {
		t.Fatalf("uploaded snapshot rows = %d/%d %+v", len(files), total, files)
	}
}

func TestEnsureConfigFileCreatesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	created, err := ensureConfigFile(path)
	if err != nil {
		t.Fatalf("ensureConfigFile: %v", err)
	}
	if !created {
		t.Fatal("expected config file to be created")
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("load created config: %v", err)
	}
	want := config.Default()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("created config mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestRestartArgumentsUsesServeForNoCommandLaunch(t *testing.T) {
	got := restartArguments([]string{"-config", "/tmp/config.json"}, true)
	want := []string{"-config", "/tmp/config.json", "serve"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restart args=%v want %v", got, want)
	}

	explicit := []string{"-profile", "archive", "serve"}
	got = restartArguments(explicit, false)
	if !reflect.DeepEqual(got, explicit) {
		t.Fatalf("explicit restart args=%v want %v", got, explicit)
	}
	got[0] = "changed"
	if explicit[0] == "changed" {
		t.Fatal("restartArguments returned caller-owned slice")
	}
}

func TestEnsureConfigFileDoesNotOverwriteExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("sentinel"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	created, err := ensureConfigFile(path)
	if err != nil {
		t.Fatalf("ensureConfigFile: %v", err)
	}
	if created {
		t.Fatal("expected existing config to be preserved")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read preserved config: %v", err)
	}
	if string(got) != "sentinel" {
		t.Fatalf("config was overwritten: %q", got)
	}
}

func TestEnsureProfileLayoutCreatesCentralAndDefaultProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	created, err := ensureProfileLayout(path)
	if err != nil {
		t.Fatalf("ensureProfileLayout: %v", err)
	}
	if !created {
		t.Fatal("expected fresh profile layout to be created")
	}
	central, err := config.LoadCentral(path)
	if err != nil {
		t.Fatalf("load central: %v", err)
	}
	if central.ActiveProfile != "default" {
		t.Fatalf("active profile = %q, want default", central.ActiveProfile)
	}
	profilePath, _ := config.ProfilePath(path, "default")
	if _, err := config.LoadProfile(profilePath); err != nil {
		t.Fatalf("load default profile: %v", err)
	}
}

func TestEnsureProfileLayoutMigratesLegacyConfigAndIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	legacy := config.Default()
	legacy.Source.LocalDir.Root = dir
	legacy.S3.Bucket = "legacy-bucket"
	if err := config.Save(path, legacy); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.db"), []byte("legacy-index"), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := ensureProfileLayout(path)
	if err != nil {
		t.Fatalf("ensureProfileLayout: %v", err)
	}
	if created {
		t.Fatal("legacy migration should not report fresh creation")
	}
	central, err := config.LoadCentral(path)
	if err != nil {
		t.Fatalf("load central: %v", err)
	}
	if central.ActiveProfile != "default" || central.Server != legacy.Server {
		t.Fatalf("central = %+v", central)
	}
	profilePath, _ := config.ProfilePath(path, "default")
	prof, err := config.LoadProfile(profilePath)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	if prof.S3.Bucket != "legacy-bucket" {
		t.Fatalf("bucket = %q", prof.S3.Bucket)
	}
	indexPath, _ := config.ProfileIndexPath(path, "default")
	got, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read migrated index: %v", err)
	}
	if string(got) != "legacy-index" {
		t.Fatalf("index content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy index still exists or stat failed: %v", err)
	}
}

func TestLoadAppStateAllowsFreshOnboardingProfile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "config.json")
	created, err := ensureProfileLayout(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected fresh profile layout")
	}
	app, err := loadAppState(ctx, path, "", false)
	if err != nil {
		t.Fatalf("load setup app state: %v", err)
	}
	defer app.close()
	if !app.setupRequired {
		t.Fatal("fresh app should require setup")
	}
	if app.src != nil || app.store != nil {
		t.Fatalf("bootstrap initialized resources: src=%v store=%v", app.src, app.store)
	}
}

func TestSetCentralPasswordHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	central := config.DefaultCentral()
	if err := config.SaveCentral(path, central); err != nil {
		t.Fatalf("save central: %v", err)
	}
	if err := setCentralPasswordHash(path, "s3cr3t"); err != nil {
		t.Fatalf("setCentralPasswordHash: %v", err)
	}

	got, err := config.LoadCentral(path)
	if err != nil {
		t.Fatalf("load central: %v", err)
	}
	if got.Auth.PasswordHash == "" {
		t.Fatal("expected password hash to be stored")
	}
	if got.Auth.PasswordHash == "s3cr3t" || strings.Contains(got.Auth.PasswordHash, "s3cr3t") {
		t.Fatalf("password leaked into stored hash: %q", got.Auth.PasswordHash)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(got.Auth.PasswordHash), []byte("s3cr3t")); err != nil {
		t.Fatalf("stored hash does not verify: %v", err)
	}
}

func TestCreateProfileCloneClearsBucketAndQueueURL(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	active := config.Default()
	active.S3.Bucket = "prod-bucket"
	active.SQS.QueueURL = "https://sqs.us-east-1.amazonaws.com/123/prod"

	a := &appState{
		cfgPath: cfgPath,
		cfg:     active,
	}

	info, err := a.createProfile(ctx, "photos", true)
	if err != nil {
		t.Fatalf("createProfile: %v", err)
	}
	if info.Bucket != "" {
		t.Fatalf("returned bucket = %q, want empty", info.Bucket)
	}

	profilePath, err := config.ProfilePath(cfgPath, "photos")
	if err != nil {
		t.Fatal(err)
	}
	prof, err := config.LoadProfile(profilePath)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	if prof.S3.Bucket != "" {
		t.Fatalf("bucket = %q, want empty", prof.S3.Bucket)
	}
	if prof.SQS.QueueURL != "" {
		t.Fatalf("queue_url = %q, want empty", prof.SQS.QueueURL)
	}
	if prof.S3.Region != active.S3.Region {
		t.Fatalf("region = %q, want cloned %q", prof.S3.Region, active.S3.Region)
	}
}

func TestAppCloseDoesNotWaitForSQSDone(t *testing.T) {
	var cancelled atomic.Bool
	a := &appState{
		sqsConsumer: &restore.Consumer{},
		sqsCancel: func() {
			cancelled.Store(true)
		},
		sqsDone: make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		a.close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("app.close blocked waiting for sqsDone")
	}

	if !cancelled.Load() {
		t.Fatal("expected sqs cancel to be called")
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.sqsConsumer != nil || a.sqsCancel != nil || a.sqsDone != nil {
		t.Fatalf("sqs state not cleared: consumer=%v cancel=%v done=%v", a.sqsConsumer, a.sqsCancel, a.sqsDone)
	}
}

type fakeShutdowner struct {
	calls   atomic.Int32
	first   context.Context
	second  context.Context
	release chan struct{}
	started chan struct{}
}

func (f *fakeShutdowner) Shutdown(ctx context.Context) error {
	switch f.calls.Add(1) {
	case 1:
		f.first = ctx
		return context.DeadlineExceeded
	case 2:
		f.second = ctx
		if f.started != nil {
			close(f.started)
		}
		<-f.release
		return nil
	default:
		return nil
	}
}

func TestWaitForShutdownRetriesWithoutDeadline(t *testing.T) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)

	fake := &fakeShutdowner{release: make(chan struct{}), started: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		waitForShutdown(fake, shutdownCtx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	select {
	case <-fake.started:
	case <-time.After(2 * time.Second):
		t.Fatal("fallback shutdown was not attempted after the timeout")
	}

	select {
	case <-done:
		t.Fatal("waitForShutdown returned before the fallback shutdown completed")
	case <-time.After(50 * time.Millisecond):
	}

	if _, ok := fake.first.Deadline(); !ok {
		t.Fatal("expected the initial shutdown to receive the bounded context")
	}
	if _, ok := fake.second.Deadline(); ok {
		t.Fatal("fallback shutdown should run without the deadline")
	}

	close(fake.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForShutdown did not return after the fallback shutdown completed")
	}
}

func TestSwitchProfileAllowsUnconfiguredS3(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	sourceDir := t.TempDir()

	central := config.DefaultCentral()
	if err := config.SaveCentral(cfgPath, central); err != nil {
		t.Fatal(err)
	}
	prof := config.DefaultProfile()
	prof.Source.LocalDir.Root = sourceDir
	prof.S3.Bucket = ""
	prof.S3.Region = ""
	prof.S3.StorageClass = ""
	profilePath, err := config.ProfilePath(cfgPath, "photos")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SaveProfile(profilePath, prof); err != nil {
		t.Fatal(err)
	}

	oldDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	oldSrc, err := source.NewLocalDir(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	a := &appState{
		cfgPath: cfgPath,
		cfg:     config.Default(),
		profile: "default",
		db:      oldDB,
		src:     oldSrc,
	}

	rt, err := a.switchProfileTo(ctx, ctx, "photos")
	if err != nil {
		t.Fatalf("switchProfileTo: %v", err)
	}
	t.Cleanup(func() { a.close() })
	if rt.Info.Name != "photos" || rt.Info.Bucket != "" {
		t.Fatalf("runtime info = %+v", rt.Info)
	}
	if a.store != nil {
		t.Fatal("store should be nil when S3 bucket is not configured")
	}
	gotCentral, err := config.LoadCentral(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotCentral.ActiveProfile != "photos" {
		t.Fatalf("active profile = %q, want photos", gotCentral.ActiveProfile)
	}
}
