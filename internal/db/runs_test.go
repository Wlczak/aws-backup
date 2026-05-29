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

	if err := d.ClearRunScanFolders(ctx, runID); err != nil {
		t.Fatalf("ClearRunScanFolders: %v", err)
	}
	got, err = d.ListRunScanFolderPaths(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunScanFolderPaths after clear: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("paths after clear=%v", got)
	}
}
