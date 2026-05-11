package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpenIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "idem.db")
	for i := 0; i < 3; i++ {
		d, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		d.Close()
	}
}

func TestUpsertFileLifecycle(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	mtime := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	seen := time.Now().UTC().Truncate(time.Second)

	r, err := d.UpsertFile(ctx, "photos/2024/a.jpg", 1000, mtime, seen)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Created || !r.Changed {
		t.Fatalf("first insert: want Created+Changed, got %+v", r)
	}

	// Same path, same size+mtime → untouched but last_seen_at updated.
	seen2 := seen.Add(time.Minute)
	r, err = d.UpsertFile(ctx, "photos/2024/a.jpg", 1000, mtime, seen2)
	if err != nil {
		t.Fatal(err)
	}
	if r.Created || r.Changed {
		t.Fatalf("no-op upsert: want unchanged, got %+v", r)
	}

	// Mark uploaded.
	if err := d.MarkUploaded(ctx, r.ID, "md5hex", "backups/photos/a.jpg", seen2); err != nil {
		t.Fatal(err)
	}

	// Size change → Changed=true, status reset to pending. The previous
	// upload's md5/s3_key/uploaded_at are intentionally preserved as the
	// historical record so reconcileFromS3 can distinguish a fresh row
	// from a modified-after-upload row (#103).
	r, err = d.UpsertFile(ctx, "photos/2024/a.jpg", 2000, mtime, seen2)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed || r.Created {
		t.Fatalf("size change: want Changed=true, Created=false, got %+v", r)
	}
	files, _, err := d.ListFiles(ctx, FilesFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.Status != StatusPending {
		t.Errorf("want status=pending after change, got %q", f.Status)
	}
	if f.MD5 != "md5hex" || f.S3Key != "backups/photos/a.jpg" {
		t.Errorf("md5/s3_key not preserved: %+v", f)
	}
	if f.UploadedAt.IsZero() {
		t.Errorf("uploaded_at unexpectedly cleared: %+v", f)
	}
}

func TestUpsertFileBatchPromotesCloudOnlyAndPreservesUploaded(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	now := time.Now().UTC().Truncate(time.Second)
	if _, err := d.UpsertFile(ctx, "cloud.txt", 10, now, now); err != nil {
		t.Fatal(err)
	}
	if err := d.g.WithContext(ctx).Model(&File{}).Where("path = ?", "cloud.txt").Updates(map[string]any{
		"status":      StatusCloudOnly,
		"s3_key":      "backups/cloud.txt",
		"uploaded_at": now,
	}).Error; err != nil {
		t.Fatalf("plant cloud_only row: %v", err)
	}
	uploadedRes, err := d.UpsertFile(ctx, "uploaded.txt", 10, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploaded(ctx, uploadedRes.ID, "md5", "backups/uploaded.txt", now); err != nil {
		t.Fatalf("plant uploaded row: %v", err)
	}

	seen := now.Add(10 * time.Minute)
	res, err := d.UpsertFileBatch(ctx, []BatchEntry{
		{Path: "cloud.txt", Size: 10, ModTime: now},
		{Path: "uploaded.txt", Size: 10, ModTime: now},
	}, seen)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Created || res[0].Changed || res[1].Created || res[1].Changed {
		t.Fatalf("unexpected upsert result: %+v", res)
	}

	files, _, err := d.ListFiles(ctx, FilesFilter{Search: "cloud.txt", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	if files[0].Status != StatusUploaded {
		t.Fatalf("cloud_only row should normalize to uploaded, got %q", files[0].Status)
	}
	if files[0].LastSeenAt != seen {
		t.Fatalf("last_seen_at = %v, want %v", files[0].LastSeenAt, seen)
	}

	uploadedFiles, _, err := d.ListFiles(ctx, FilesFilter{Search: "uploaded.txt", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(uploadedFiles) != 1 {
		t.Fatalf("want 1 uploaded file, got %d", len(uploadedFiles))
	}
	if uploadedFiles[0].Status != StatusUploaded {
		t.Fatalf("uploaded row should stay uploaded, got %q", uploadedFiles[0].Status)
	}
}

func TestSnapshotToProducesStableCopy(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	now := time.Now().UTC()
	r1, err := d.UpsertFile(ctx, "a.txt", 1, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploaded(ctx, r1.ID, "m1", "k1", now); err != nil {
		t.Fatal(err)
	}

	snapPath := filepath.Join(t.TempDir(), "snapshot.db")
	if err := d.SnapshotTo(ctx, snapPath); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}

	// Mutate the live DB after the snapshot so we can prove the copy is
	// a stable point-in-time image, not a handle to the live file.
	r2, err := d.UpsertFile(ctx, "b.txt", 2, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkUploaded(ctx, r2.ID, "m2", "k2", now); err != nil {
		t.Fatal(err)
	}

	snap, err := Open(ctx, snapPath)
	if err != nil {
		t.Fatalf("Open snapshot: %v", err)
	}
	t.Cleanup(func() { _ = snap.Close() })

	files, total, err := snap.ListFiles(ctx, FilesFilter{})
	if err != nil {
		t.Fatalf("ListFiles snapshot: %v", err)
	}
	if total != 1 || len(files) != 1 {
		t.Fatalf("snapshot row count = %d/%d, want 1/1", len(files), total)
	}
	if files[0].Path != "a.txt" || files[0].Status != StatusUploaded {
		t.Fatalf("snapshot row = %+v, want uploaded a.txt", files[0])
	}
}

func TestMarkMissing(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	old := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	new := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)

	// Uploaded, old last_seen_at — should be marked missing.
	r, _ := d.UpsertFile(ctx, "a.txt", 1, old, old)
	_ = d.MarkUploaded(ctx, r.ID, "m", "k", old)

	// Uploaded, recent last_seen_at — stays uploaded.
	r2, _ := d.UpsertFile(ctx, "b.txt", 1, old, new)
	_ = d.MarkUploaded(ctx, r2.ID, "m", "k", new)

	// Pending, old — should be marked missing so deleted-but-queued
	// rows don't sit in the queue forever.
	_, _ = d.UpsertFile(ctx, "c.txt", 1, old, old)

	// Zipped (SetZipName succeeded, MarkUploadedBatch not yet), old — should be marked missing.
	r3, _ := d.UpsertFile(ctx, "d.txt", 1, old, old)
	_ = d.SetZipName(ctx, []int64{r3.ID}, "z.zip")

	// Failed, old — should be marked missing (was queued but file is gone).
	r4, _ := d.UpsertFile(ctx, "e.txt", 1, old, old)
	_ = d.MarkFailed(ctx, r4.ID)

	// Failed, recent — should stay failed (file still being seen).
	r5, _ := d.UpsertFile(ctx, "f.txt", 1, old, new)
	_ = d.MarkFailed(ctx, r5.ID)

	// Cloud-only, old — should stay recoverable from S3 and not be
	// collapsed back to missing by the source-side scan.
	r6, _ := d.UpsertFile(ctx, "g.txt", 1, old, old)
	_ = d.g.WithContext(ctx).Model(&File{}).Where("id = ?", r6.ID).Updates(map[string]any{
		"status":      StatusCloudOnly,
		"s3_key":      "backups/g.txt",
		"uploaded_at": old,
	}).Error

	affected, err := d.MarkMissing(ctx, new)
	if err != nil {
		t.Fatal(err)
	}
	if affected != 4 {
		t.Errorf("want 4 affected, got %d", affected)
	}

	files, _, _ := d.ListFiles(ctx, FilesFilter{All: true})
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = f.Status
	}
	if got["a.txt"] != StatusMissing {
		t.Errorf("a.txt want missing, got %q", got["a.txt"])
	}
	if got["b.txt"] != StatusUploaded {
		t.Errorf("b.txt want uploaded, got %q", got["b.txt"])
	}
	if got["c.txt"] != StatusMissing {
		t.Errorf("c.txt want missing, got %q", got["c.txt"])
	}
	if got["d.txt"] != StatusMissing {
		t.Errorf("d.txt want missing, got %q", got["d.txt"])
	}
	if got["e.txt"] != StatusMissing {
		t.Errorf("e.txt want missing, got %q", got["e.txt"])
	}
	if got["f.txt"] != StatusFailed {
		t.Errorf("f.txt want failed, got %q", got["f.txt"])
	}
	if got["g.txt"] != StatusCloudOnly {
		t.Errorf("g.txt want cloud_only, got %q", got["g.txt"])
	}
}

func TestListFilesFilter(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	now := time.Now().UTC()
	for _, p := range []string{"alpha.txt", "beta.txt", "gamma.txt", "delta.txt"} {
		_, err := d.UpsertFile(ctx, p, 1, now, now)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Page 1, limit 2 → 2 rows, total 4.
	files, total, err := d.ListFiles(ctx, FilesFilter{Page: 1, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || len(files) != 2 {
		t.Fatalf("want 2/4, got %d/%d", len(files), total)
	}

	// Search.
	files, _, _ = d.ListFiles(ctx, FilesFilter{Search: "amma"})
	if len(files) != 1 || files[0].Path != "gamma.txt" {
		t.Errorf("search 'amma' want gamma.txt, got %+v", files)
	}
}

// TestListFilesFilterLikeEscaping covers #67: LIKE wildcards in user
// search input must be treated as literal characters, not pattern
// metacharacters. Without escaping, "?search=%" returned every row.
func TestListFilesFilterLikeEscaping(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()
	for _, p := range []string{
		"alpha.txt",
		"with%percent.txt",
		"with_underscore.txt",
	} {
		if _, err := d.UpsertFile(ctx, p, 1, now, now); err != nil {
			t.Fatal(err)
		}
	}

	// "%" alone must NOT match every row — it's a literal now.
	_, total, err := d.ListFiles(ctx, FilesFilter{Search: "%"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("search '%%' total=%d want 1 (literal match)", total)
	}

	// "_" alone must match only the file with a literal underscore, not
	// any single-character substring.
	_, total, _ = d.ListFiles(ctx, FilesFilter{Search: "_"})
	if total != 1 {
		t.Errorf("search '_' total=%d want 1 (literal match)", total)
	}

	// Plain substring search must still work.
	files, _, _ := d.ListFiles(ctx, FilesFilter{Search: "alpha"})
	if len(files) != 1 || files[0].Path != "alpha.txt" {
		t.Errorf("search 'alpha' want alpha.txt, got %+v", files)
	}
}

func TestSetZipName(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	now := time.Now().UTC()
	r1, _ := d.UpsertFile(ctx, "dir/a.txt", 1, now, now)
	r2, _ := d.UpsertFile(ctx, "dir/b.txt", 1, now, now)
	r3, _ := d.UpsertFile(ctx, "other/c.txt", 1, now, now)

	if err := d.SetZipName(ctx, []int64{r1.ID, r2.ID}, "dir_20240101.zip"); err != nil {
		t.Fatal(err)
	}

	files, _, _ := d.ListFiles(ctx, FilesFilter{})
	want := map[string]string{"dir/a.txt": "dir_20240101.zip", "dir/b.txt": "dir_20240101.zip", "other/c.txt": ""}
	for _, f := range files {
		if f.ZipName != want[f.Path] {
			t.Errorf("%s zip_name=%q, want %q", f.Path, f.ZipName, want[f.Path])
		}
	}
	// r3 untouched → still pending.
	for _, f := range files {
		if f.Path == "other/c.txt" && f.Status != StatusPending {
			t.Errorf("other/c.txt status=%q, want pending", f.Status)
		}
		if f.Path != "other/c.txt" && f.Status != StatusZipped {
			t.Errorf("%s status=%q, want zipped", f.Path, f.Status)
		}
	}
	_ = r3
}

func TestStats(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	now := time.Now().UTC()
	r, _ := d.UpsertFile(ctx, "a.txt", 100, now, now)
	_ = d.MarkUploaded(ctx, r.ID, "m", "k", now)
	_, _ = d.UpsertFile(ctx, "b.txt", 200, now, now)
	_, _ = d.UpsertFile(ctx, "c.txt", 300, now, now)

	s, err := d.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalCount != 3 {
		t.Errorf("count want 3, got %d", s.TotalCount)
	}
	if s.TotalSize != 600 {
		t.Errorf("bytes want 600, got %d", s.TotalSize)
	}
	if s.ByStatus[StatusUploaded] != 1 || s.ByStatus[StatusPending] != 2 {
		t.Errorf("by-status wrong: %+v", s.ByStatus)
	}
}

func TestRunLifecycle(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	started := time.Now().UTC().Truncate(time.Second)
	id, err := d.CreateRun(ctx, started)
	if err != nil {
		t.Fatal(err)
	}

	r, err := d.GetRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != RunRunning || !r.FinishedAt.IsZero() {
		t.Errorf("unexpected initial run: %+v", r)
	}

	if err := d.UpdateRunStats(ctx, id, 10, 5, 1024); err != nil {
		t.Fatal(err)
	}
	if err := d.AppendLog(ctx, id, LogInfo, "hello", started); err != nil {
		t.Fatal(err)
	}
	if err := d.AppendLog(ctx, id, LogError, "boom", started.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	logs, _, err := d.ListLogs(ctx, id, 1, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 || logs[0].Message != "hello" || logs[1].Level != LogError {
		t.Errorf("log ordering/content wrong: %+v", logs)
	}

	finished := started.Add(time.Minute)
	if err := d.FinishRun(ctx, id, RunFailed, "kaboom", finished); err != nil {
		t.Fatal(err)
	}
	r, _ = d.GetRun(ctx, id)
	if r.Status != RunFailed || r.ErrorMessage != "kaboom" || r.FinishedAt.IsZero() {
		t.Errorf("bad finished run: %+v", r)
	}

	runs, total, err := d.ListRuns(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(runs) != 1 {
		t.Errorf("want 1/1 runs, got %d/%d", len(runs), total)
	}
}

func TestTrimRunLogsForRun(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	started := time.Now().UTC().Truncate(time.Second)
	id, err := d.CreateRun(ctx, started)
	if err != nil {
		t.Fatal(err)
	}

	// Seed: 5 info, 2 warn, 1 error — 8 rows total.
	for i := 0; i < 5; i++ {
		if err := d.AppendLog(ctx, id, LogInfo, "i", started.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AppendLog(ctx, id, LogWarn, "w1", started.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := d.AppendLog(ctx, id, LogWarn, "w2", started.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := d.AppendLog(ctx, id, LogError, "e1", started.Add(12*time.Second)); err != nil {
		t.Fatal(err)
	}

	// Cap to 4 — must drop 4 rows. Lowest-severity (info) oldest first
	// means the 4 oldest info rows go; remaining: 1 info, 2 warn, 1 error.
	deleted, err := d.TrimRunLogsForRun(ctx, id, 4)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 4 {
		t.Errorf("want 4 deleted, got %d", deleted)
	}
	logs, total, err := d.ListLogs(ctx, id, 1, 500)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Errorf("want 4 remaining, got %d", total)
	}
	var info, warn, errLevel int
	for _, l := range logs {
		switch l.Level {
		case LogInfo:
			info++
		case LogWarn:
			warn++
		case LogError:
			errLevel++
		}
	}
	if errLevel != 1 || warn != 2 || info != 1 {
		t.Errorf("severity preservation broken: info=%d warn=%d error=%d", info, warn, errLevel)
	}

	// Cap >= total → no-op.
	deleted, err = d.TrimRunLogsForRun(ctx, id, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("want 0 deleted on no-op, got %d", deleted)
	}

	// maxLines<=0 → no-op.
	deleted, err = d.TrimRunLogsForRun(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("want 0 deleted on disabled, got %d", deleted)
	}
}

func TestTrimRunLogsByAge(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-60 * 24 * time.Hour)   // 60 days ago
	recent := now.Add(-1 * 24 * time.Hour) // 1 day ago

	oldID, err := d.CreateRun(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	recentID, err := d.CreateRun(ctx, recent)
	if err != nil {
		t.Fatal(err)
	}
	activeID, err := d.CreateRun(ctx, now)
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []int64{oldID, recentID, activeID} {
		if err := d.AppendLog(ctx, id, LogInfo, "x", now); err != nil {
			t.Fatal(err)
		}
	}
	// Finish the two non-active ones with appropriate timestamps.
	if err := d.FinishRun(ctx, oldID, RunCompleted, "", old.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishRun(ctx, recentID, RunCompleted, "", recent.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Cutoff = 30 days ago. Old run's logs go; recent + active stay.
	cutoff := now.Add(-30 * 24 * time.Hour)
	deleted, err := d.TrimRunLogsByAge(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("want 1 deleted, got %d", deleted)
	}

	// Old run row must still exist (only logs were trimmed).
	if _, err := d.GetRun(ctx, oldID); err != nil {
		t.Fatalf("old run row was unexpectedly deleted: %v", err)
	}
	if _, total, _ := d.ListLogs(ctx, oldID, 1, 10); total != 0 {
		t.Errorf("old run still has %d log rows", total)
	}
	if _, total, _ := d.ListLogs(ctx, recentID, 1, 10); total != 1 {
		t.Errorf("recent run logs trimmed unexpectedly: %d", total)
	}
	if _, total, _ := d.ListLogs(ctx, activeID, 1, 10); total != 1 {
		t.Errorf("active run logs trimmed unexpectedly: %d", total)
	}

	// Zero cutoff → no-op.
	deleted, err = d.TrimRunLogsByAge(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("want 0 deleted on disabled, got %d", deleted)
	}
}

func TestListPending(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()

	// seed: a.txt pending, b.txt failed, c.txt uploaded
	ra, _ := d.UpsertFile(ctx, "a.txt", 1, now, now)
	rb, _ := d.UpsertFile(ctx, "b.txt", 1, now, now)
	rc, _ := d.UpsertFile(ctx, "c.txt", 1, now, now)
	_ = d.MarkFailed(ctx, rb.ID)
	_ = d.MarkUploaded(ctx, rc.ID, "m", "k", now)

	// includeFailed=false -> only 'a'
	rows, err := d.ListPending(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "a.txt" {
		t.Fatalf("pending-only: %+v", rows)
	}

	// includeFailed=true -> 'a' + 'b'
	rows, err = d.ListPending(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	gotPaths := map[string]bool{}
	for _, r := range rows {
		gotPaths[r.Path] = true
	}
	if !gotPaths["a.txt"] || !gotPaths["b.txt"] || gotPaths["c.txt"] {
		t.Fatalf("includeFailed: %+v", gotPaths)
	}
	_ = ra
}

func TestMarkPendingByIDs(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()
	r, _ := d.UpsertFile(ctx, "a.txt", 10, now, now)
	_ = d.MarkUploaded(ctx, r.ID, "m", "k", now)

	n, err := d.MarkPendingByIDs(ctx, []int64{r.ID})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("affected=%d want 1", n)
	}
	files, _, _ := d.ListFiles(ctx, FilesFilter{})
	if files[0].Status != StatusPending || files[0].MD5 != "" || files[0].S3Key != "" {
		t.Errorf("not fully reset: %+v", files[0])
	}
}

func TestMarkAllFailedPending(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()
	r1, _ := d.UpsertFile(ctx, "a.txt", 1, now, now)
	r2, _ := d.UpsertFile(ctx, "b.txt", 1, now, now)
	r3, _ := d.UpsertFile(ctx, "c.txt", 1, now, now)
	_ = d.MarkFailed(ctx, r1.ID)
	_ = d.MarkFailed(ctx, r2.ID)
	_ = d.MarkUploaded(ctx, r3.ID, "m", "k", now)

	n, err := d.MarkAllFailedPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("affected=%d want 2", n)
	}
	files, _, _ := d.ListFiles(ctx, FilesFilter{})
	statuses := map[string]string{}
	for _, f := range files {
		statuses[f.Path] = f.Status
	}
	if statuses["a.txt"] != StatusPending || statuses["b.txt"] != StatusPending {
		t.Errorf("failed rows not flipped: %+v", statuses)
	}
	if statuses["c.txt"] != StatusUploaded {
		t.Errorf("uploaded row touched: %+v", statuses)
	}
}

func TestDeleteFiles(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()
	r1, _ := d.UpsertFile(ctx, "a.txt", 1, now, now)
	r2, _ := d.UpsertFile(ctx, "b.txt", 1, now, now)
	_, _ = d.UpsertFile(ctx, "c.txt", 1, now, now)

	n, err := d.DeleteFiles(ctx, []int64{r1.ID, r2.ID})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("affected=%d want 2", n)
	}
	_, total, _ := d.ListFiles(ctx, FilesFilter{})
	if total != 1 {
		t.Errorf("total=%d want 1", total)
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	_, ok, err := d.GetSetting(ctx, "nope")
	if err != nil || ok {
		t.Fatalf("missing key: err=%v ok=%v", err, ok)
	}

	if err := d.SetSetting(ctx, "schedule", "0 2 * * *"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := d.GetSetting(ctx, "schedule")
	if err != nil || !ok || v != "0 2 * * *" {
		t.Errorf("get: v=%q ok=%v err=%v", v, ok, err)
	}

	// Overwrite.
	_ = d.SetSetting(ctx, "schedule", "*/5 * * * *")
	v, _, _ = d.GetSetting(ctx, "schedule")
	if v != "*/5 * * * *" {
		t.Errorf("overwrite failed: %q", v)
	}
}
