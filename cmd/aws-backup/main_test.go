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
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/source"
	"github.com/Wlczak/aws-backup/internal/storage"
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
