package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestFileRevisionTracksWritesOnly(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "revision.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	initial := database.FileRevision()
	created, err := database.CreateFiles(ctx, []File{{
		Path: "one.txt", Size: 1, MTime: time.Now().UTC(), Status: StatusPending, LastSeenAt: time.Now().UTC(),
	}})
	if err != nil || created != 1 {
		t.Fatalf("CreateFiles created=%d err=%v", created, err)
	}
	afterCreate := database.FileRevision()
	if afterCreate <= initial {
		t.Fatalf("revision after create=%d initial=%d", afterCreate, initial)
	}

	if _, _, err := database.ListFiles(ctx, FilesFilter{Page: 1, Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if got := database.FileRevision(); got != afterCreate {
		t.Fatalf("read changed revision to %d want %d", got, afterCreate)
	}

	rows, _, err := database.ListFiles(ctx, FilesFilter{Page: 1, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkPendingByIDs(ctx, []int64{rows[0].ID}); err != nil {
		t.Fatal(err)
	}
	// The row was already pending, so SQLite may report it unchanged. Use a
	// status transition to guarantee an affected update callback.
	if err := database.MarkFailed(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	afterUpdate := database.FileRevision()
	if afterUpdate <= afterCreate {
		t.Fatalf("revision after update=%d after create=%d", afterUpdate, afterCreate)
	}

	if _, err := database.DeleteFiles(ctx, []int64{rows[0].ID}); err != nil {
		t.Fatal(err)
	}
	if got := database.FileRevision(); got <= afterUpdate {
		t.Fatalf("revision after delete=%d after update=%d", got, afterUpdate)
	}
}
