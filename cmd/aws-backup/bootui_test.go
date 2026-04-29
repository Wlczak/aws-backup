package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/storage"
)

func TestBootDownload_ServesHTMLAndCompletes(t *testing.T) {
	store := storage.NewMemStorage()
	body := bytes.Repeat([]byte("x"), 256<<10) // 256 KiB
	if _, err := store.PutStandard(context.Background(), "index.db", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "index.db")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	// Drive the download from a goroutine so the main test body can
	// exercise the HTTP handlers concurrently.
	done := make(chan error, 1)
	go func() {
		done <- runBootDownload(context.Background(), ln, store, "", dst, int64(len(body)))
	}()

	// GET / should serve the HTML page.
	res, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	pageBody, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("GET / status=%d want 200", res.StatusCode)
	}
	for _, want := range []string{"Downloading index.db from S3", "/progress", "/cancel"} {
		if !strings.Contains(string(pageBody), want) {
			t.Errorf("HTML missing %q", want)
		}
	}

	// Wait for the download goroutine to finish.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runBootDownload: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runBootDownload did not return within 5s")
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("dst content mismatch: got %d bytes, want %d", len(got), len(body))
	}
}

// slowReader wraps an io.Reader and pauses each Read so the boot UI's
// cancel path has time to fire. Used only by the cancel-path test.
type slowReader struct {
	r     io.Reader
	delay time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	// Cap each Read to a small chunk so tot. transfer time is dominated by
	// (numReads * delay), giving the cancel button room to land.
	if len(p) > 4096 {
		p = p[:4096]
	}
	time.Sleep(s.delay)
	return s.r.Read(p)
}

// slowStorage wraps a storage.Storage so Get returns a body that drips bytes
// out slowly enough for the cancel-path test to exercise mid-download cancel.
type slowStorage struct {
	storage.Storage
	delay time.Duration
}

func (s *slowStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.Storage.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return &slowReadCloser{r: &slowReader{r: rc, delay: s.delay}, c: rc}, nil
}

type slowReadCloser struct {
	r io.Reader
	c io.Closer
}

func (s *slowReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *slowReadCloser) Close() error               { return s.c.Close() }

func TestBootDownload_CancelButtonAbortsDownload(t *testing.T) {
	mem := storage.NewMemStorage()
	body := bytes.Repeat([]byte("y"), 256<<10) // 256 KiB
	if _, err := mem.PutStandard(context.Background(), "index.db", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store := &slowStorage{Storage: mem, delay: 20 * time.Millisecond}
	dst := filepath.Join(t.TempDir(), "index.db")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	var wg sync.WaitGroup
	var dlErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		dlErr = runBootDownload(context.Background(), ln, store, "", dst, int64(len(body)))
	}()

	// Wait until /progress reports some bytes flowing so we know the cancel
	// fires mid-download (not before the first read).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get("http://" + addr + "/progress")
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var p struct {
			Bytes int64 `json:"bytes"`
			Done  bool  `json:"done"`
		}
		_ = json.NewDecoder(res.Body).Decode(&p)
		res.Body.Close()
		if p.Done {
			t.Fatal("download completed before we could fire cancel")
		}
		if p.Bytes > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	res, err := http.Post("http://"+addr+"/cancel", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatalf("POST /cancel: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("POST /cancel status=%d want 200", res.StatusCode)
	}

	wg.Wait()
	// Cancel via the button is treated as soft-success: returns nil and
	// leaves whatever local state existed (none here, so dst is absent).
	if dlErr != nil {
		t.Errorf("runBootDownload after user-cancel: got %v want nil", dlErr)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst should not exist after cancel before completion, stat err=%v", err)
	}
}
