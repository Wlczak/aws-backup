package api

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
)

func TestFileStatsCacheInvalidatesOnDatabaseMutation(t *testing.T) {
	ts, deps := newTestServer(t)

	var before fileStatsResponse
	resp := getJSON(t, ts, "/api/files/stats", &before)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initial status=%d want 200", resp.StatusCode)
	}
	if before.TotalCount != 0 {
		t.Fatalf("initial total=%d want 0", before.TotalCount)
	}
	now := time.Now().UTC()
	if _, err := deps.DB.CreateFiles(context.Background(), []db.File{{
		Path: "new.txt", Size: 42, MTime: now, Status: db.StatusPending, LastSeenAt: now,
	}}); err != nil {
		t.Fatal(err)
	}

	var after fileStatsResponse
	resp = getJSON(t, ts, "/api/files/stats", &after)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("updated status=%d want 200", resp.StatusCode)
	}
	if after.TotalCount != 1 || after.TotalSize != 42 {
		t.Fatalf("updated stats=%+v want count=1 size=42", after)
	}
}

func TestFileCacheKeysNormalizeIgnoredAllPagination(t *testing.T) {
	ts, deps := newTestServer(t)
	now := time.Now().UTC()
	if _, err := deps.DB.CreateFiles(context.Background(), []db.File{{
		Path: filepath.ToSlash("docs/readme.txt"), Size: 1, MTime: now,
		Status: db.StatusPending, LastSeenAt: now,
	}}); err != nil {
		t.Fatal(err)
	}

	firstPath := "/api/files?all=true&page=1&limit=50"
	resp, err := ts.Client().Get(ts.URL + firstPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s status=%d want 200", firstPath, resp.StatusCode)
	}

	// A different page/limit pair is ignored for all=true and therefore has
	// the same normalized cache key. Closing the DB proves the second response
	// comes from that entry rather than another query.
	if err := deps.DB.Close(); err != nil {
		t.Fatal(err)
	}
	secondPath := "/api/files?limit=999&page=99&all=true"
	resp, err = ts.Client().Get(ts.URL + secondPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s status=%d want cached 200", secondPath, resp.StatusCode)
	}
}
