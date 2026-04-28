package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
