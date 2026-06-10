package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openRunsTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestRunScanFolderLifecycle(t *testing.T) {
	ctx := context.Background()
	d := openRunsTestDB(t)
	runID, err := d.CreateRun(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	now := time.Now().UTC()
	if err := d.MarkRunScanFoldersComplete(ctx, runID, []string{"photos", "docs/sub"}, now); err != nil {
		t.Fatalf("MarkRunScanFoldersComplete: %v", err)
	}

	got, err := d.ListRunScanFolderPaths(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunScanFolderPaths: %v", err)
	}
	if len(got) != 2 || got[0] != "docs/sub" || got[1] != "photos" {
		t.Fatalf("paths=%v", got)
	}

	if err := d.FinishRun(ctx, runID, RunCompleted, "", now.Add(time.Minute)); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	got, err = d.ListRunScanFolderPaths(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunScanFolderPaths after finish: %v", err)
	}
	if len(got) != 2 || got[0] != "docs/sub" || got[1] != "photos" {
		t.Fatalf("paths after finish=%v", got)
	}

	if err := d.UpdateRunScanState(ctx, runID, true, false); err != nil {
		t.Fatalf("UpdateRunScanState: %v", err)
	}
	run, err := d.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !run.ScanPaused || run.ScanComplete {
		t.Fatalf("scan state = paused=%v complete=%v", run.ScanPaused, run.ScanComplete)
	}

	runID2, err := d.CreateRun(ctx, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("CreateRun #2: %v", err)
	}
	if err := d.MarkRunScanFoldersComplete(ctx, runID2, []string{"keep"}, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("MarkRunScanFoldersComplete #2: %v", err)
	}
	got, err = d.ListRunScanFolderPaths(ctx, runID2)
	if err != nil {
		t.Fatalf("ListRunScanFolderPaths run2: %v", err)
	}
	if len(got) != 1 || got[0] != "keep" {
		t.Fatalf("run2 paths=%v", got)
	}

	cached, err := d.ListCompletedScanFolderPaths(ctx)
	if err != nil {
		t.Fatalf("ListCompletedScanFolderPaths: %v", err)
	}
	if len(cached) != 3 || cached[0] != "docs/sub" || cached[1] != "keep" || cached[2] != "photos" {
		t.Fatalf("cached paths=%v", cached)
	}
}
