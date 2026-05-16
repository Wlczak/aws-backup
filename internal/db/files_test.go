package db

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestStatsToleratesNonCanonicalRestoreExpiresAt seeds a `restored`
// row whose restore_expires_at column is in the legacy
// `time.Time.String()` text format (the shape that pre-migrations DBs
// in this project carry). Before #182 follow-up, the MIN aggregate
// then surfaced an unparseable string and `sql.NullTime.Scan` 500'd
// the dashboard. Stats() must now skip the bad row gracefully.
func TestStatsToleratesNonCanonicalRestoreExpiresAt(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()

	// Seed a real row first so the rest of Stats has something to count.
	rowID, err := d.UpsertFile(ctx, "ok.bin", 100, now, now)
	if err != nil {
		t.Fatalf("seed ok row: %v", err)
	}
	if err := d.MarkUploadedBatch(ctx, []int64{rowID.ID}, "x", "k", now); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}

	// Direct UPDATE bypasses GORM's time formatter so we can plant the
	// "bad" string. Use the legacy Go-default layout that
	// `glebarez/sqlite` won't auto-parse back into time.Time.
	rawExp := time.Now().Add(48 * time.Hour).UTC().Format("2006-01-02 15:04:05.999999999 -0700 MST")
	if err := d.g.WithContext(ctx).
		Exec(`UPDATE files SET restore_status='restored', restore_expires_at=? WHERE id=?`, rawExp, rowID.ID).
		Error; err != nil {
		t.Fatalf("inject legacy timestamp: %v", err)
	}

	// Reproduce the original failure first to make sure the test
	// actually exercises the dangerous path: a NullTime scan should
	// error with "string into *time.Time".
	{
		var canary sql.NullTime
		err := d.g.WithContext(ctx).Model(&File{}).
			Select("MIN(restore_expires_at)").
			Where("restore_status = ? AND restore_expires_at IS NOT NULL", RestoreStatusRestored).
			Scan(&canary).Error
		if err == nil {
			t.Skip("driver parsed the legacy format; nothing to harden against on this build")
		}
	}

	// The fix: Stats() routes through parseFlexTime, so this should
	// not error.
	stats, err := d.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats with legacy-format restore_expires_at returned: %v", err)
	}
	if stats.RestoreSoonestExp == nil {
		t.Errorf("expected legacy timestamp to round-trip via parseFlexTime; got nil RestoreSoonestExp")
	}
}

// TestListTreeChildren asserts the lazy-tree partitioner: rows whose
// path has no further '/' beyond the prefix become direct files; rows
// with deeper paths roll up under the first segment as a folder with
// aggregate counts.
func TestListTreeChildren(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()

	seed := []struct {
		path string
		size int64
	}{
		{"top.txt", 10},
		{"alpha/a1.txt", 100},
		{"alpha/a2.txt", 200},
		{"alpha/sub/deep.txt", 1000},
		{"beta/b1.txt", 50},
	}
	for _, s := range seed {
		if _, err := d.UpsertFile(ctx, s.path, s.size, now, now); err != nil {
			t.Fatalf("seed %s: %v", s.path, err)
		}
	}

	folders, files, err := d.ListTreeChildren(ctx, "", "")
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	if len(files) != 1 || files[0].Path != "top.txt" {
		t.Errorf("root files = %v, want [top.txt]", files)
	}
	if len(folders) != 2 {
		t.Fatalf("root folders = %d, want 2", len(folders))
	}
	for _, fd := range folders {
		switch fd.Path {
		case "alpha":
			if fd.FileCount != 3 || fd.TotalSize != 1300 {
				t.Errorf("alpha aggregate = (%d, %d), want (3, 1300)", fd.FileCount, fd.TotalSize)
			}
		case "beta":
			if fd.FileCount != 1 || fd.TotalSize != 50 {
				t.Errorf("beta aggregate = (%d, %d), want (1, 50)", fd.FileCount, fd.TotalSize)
			}
		default:
			t.Errorf("unexpected folder %q", fd.Path)
		}
	}

	folders, files, err = d.ListTreeChildren(ctx, "alpha", "")
	if err != nil {
		t.Fatalf("alpha: %v", err)
	}
	if len(folders) != 1 || folders[0].Path != "alpha/sub" || folders[0].FileCount != 1 {
		t.Errorf("alpha folders = %+v, want [alpha/sub fc=1]", folders)
	}
	if len(files) != 2 {
		t.Errorf("alpha files = %d, want 2", len(files))
	}
}

// TestListSubtreeIDs verifies that selecting a folder reaches every
// descendant file regardless of nesting depth.
func TestListSubtreeIDs(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()
	for _, p := range []string{"x/1.txt", "x/y/2.txt", "x/y/z/3.txt", "other/4.txt"} {
		if _, err := d.UpsertFile(ctx, p, 1, now, now); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	ids, paths, total, err := d.ListSubtreeIDs(ctx, "x", "", 0)
	if err != nil {
		t.Fatalf("ListSubtreeIDs: %v", err)
	}
	if total != 3 || len(ids) != 3 || len(paths) != 3 {
		t.Errorf("got (ids=%d paths=%d total=%d), want 3/3/3", len(ids), len(paths), total)
	}
}

func TestParseFlexTime(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
	}{
		{"", false},
		{"   ", false},
		{"2026-05-06T14:00:00Z", true},
		{"2026-05-06T14:00:00.123456789Z", true},
		{"2026-05-06T14:00:00+02:00", true},
		{"2026-05-06 14:00:00", true},
		{"2026-05-06 14:00:00.123456789 +0000 UTC", true},
		{"2026-05-06", true},
		{"not a date", false},
	}
	for _, c := range cases {
		_, ok := parseFlexTime(c.in)
		if ok != c.wantOK {
			t.Errorf("parseFlexTime(%q): ok=%v want=%v", c.in, ok, c.wantOK)
		}
	}
}

// TestListRestoreScanKeys keeps the full restore scan keyed to actual
// uploaded objects: standalone uploads, zip archives, and cloud_only
// rows are included, while pending rows are skipped even if they still
// carry an s3_key.
func TestListRestoreScanKeys(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC()

	standalone, err := d.UpsertFile(ctx, "solo.txt", 10, now, now)
	if err != nil {
		t.Fatalf("seed standalone: %v", err)
	}
	if err := d.MarkUploaded(ctx, standalone.ID, "md5", "backups/solo.txt", now); err != nil {
		t.Fatalf("mark standalone uploaded: %v", err)
	}

	zipped, err := d.UpsertFile(ctx, "docs/spec.pdf", 20, now, now)
	if err != nil {
		t.Fatalf("seed zipped: %v", err)
	}
	if err := d.SetZipName(ctx, []int64{zipped.ID}, "docs/docs_1.zip"); err != nil {
		t.Fatalf("set zip name: %v", err)
	}
	if err := d.MarkUploaded(ctx, zipped.ID, "md5", "backups/docs/docs_1.zip", now); err != nil {
		t.Fatalf("mark zipped uploaded: %v", err)
	}

	cloudOnly, err := d.UpsertFile(ctx, "recovered.txt", 40, now, now)
	if err != nil {
		t.Fatalf("seed cloud only: %v", err)
	}
	if err := d.g.WithContext(ctx).Model(&File{}).Where("id = ?", cloudOnly.ID).Updates(map[string]any{
		"status":      StatusCloudOnly,
		"s3_key":      "backups/recovered.txt",
		"uploaded_at": now,
	}).Error; err != nil {
		t.Fatalf("plant cloud-only row: %v", err)
	}

	pending, err := d.UpsertFile(ctx, "stale.txt", 30, now, now)
	if err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	if err := d.g.WithContext(ctx).Model(&File{}).Where("id = ?", pending.ID).Updates(map[string]any{
		"status":   StatusPending,
		"s3_key":   "backups/stale.zip",
		"zip_name": "",
	}).Error; err != nil {
		t.Fatalf("plant stale pending row: %v", err)
	}

	keys, err := d.ListRestoreScanKeys(ctx)
	if err != nil {
		t.Fatalf("ListRestoreScanKeys: %v", err)
	}
	want := []string{"backups/docs/docs_1.zip", "backups/recovered.txt", "backups/solo.txt"}
	if len(keys) != len(want) {
		t.Fatalf("keys=%v want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys=%v want %v", keys, want)
		}
	}
}

// TestMarkZipUploadedBatchCreatesZipRow verifies the new archive table and
// per-file checksum split: the zip row carries archive metadata while each
// member row keeps its own MD5 and points at the zip record.
func TestMarkZipUploadedBatchCreatesZipRow(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	a, err := d.UpsertFile(ctx, "photos/a.jpg", 10, now, now)
	if err != nil {
		t.Fatalf("seed a: %v", err)
	}
	b, err := d.UpsertFile(ctx, "photos/b.jpg", 20, now, now)
	if err != nil {
		t.Fatalf("seed b: %v", err)
	}

	zipTime := now.Add(2 * time.Minute)
	if err := d.MarkZipUploadedBatch(ctx, Zip{
		ZipName:    "photos/photos_1.zip",
		Size:       1234,
		MD5:        "archive-md5",
		SHA256:     "archive-sha",
		S3Key:      "backups/photos/photos_1.zip",
		UploadedAt: &zipTime,
		LastSeenAt: zipTime,
	}, []ZipMemberUpload{
		{ID: a.ID, MD5: "md5-a"},
		{ID: b.ID, MD5: "md5-b"},
	}); err != nil {
		t.Fatalf("MarkZipUploadedBatch: %v", err)
	}

	var zipRow Zip
	if err := d.g.WithContext(ctx).Where("zip_name = ?", "photos/photos_1.zip").First(&zipRow).Error; err != nil {
		t.Fatalf("load zip row: %v", err)
	}
	if zipRow.MD5 != "archive-md5" || zipRow.SHA256 != "archive-sha" || zipRow.S3Key != "backups/photos/photos_1.zip" {
		t.Fatalf("zip row = %+v", zipRow)
	}

	files, _, err := d.ListFiles(ctx, FilesFilter{Search: "photos/", All: true})
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	for _, f := range files {
		if f.ZipID == nil {
			t.Fatalf("%s missing zip_id", f.Path)
		}
		if f.ZipName != "photos/photos_1.zip" {
			t.Fatalf("%s zip_name=%q", f.Path, f.ZipName)
		}
		wantMD5 := map[string]string{"photos/a.jpg": "md5-a", "photos/b.jpg": "md5-b"}[f.Path]
		if f.MD5 != wantMD5 {
			t.Fatalf("%s md5=%q want %q", f.Path, f.MD5, wantMD5)
		}
	}
}

func TestDownloadMirrorSnapshotLifecycle(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	first := DownloadMirrorSnapshot{
		DownloadDir:  "/tmp/mirror",
		ScannedAt:    time.Now().UTC().Truncate(time.Second),
		TotalCount:   10,
		PresentCount: 7,
		MissingCount: 3,
	}
	if err := d.UpsertDownloadMirrorSnapshot(ctx, first); err != nil {
		t.Fatalf("upsert snapshot: %v", err)
	}
	got, found, err := d.GetDownloadMirrorSnapshot(ctx, first.DownloadDir)
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if !found {
		t.Fatal("snapshot not found")
	}
	if got.DownloadDir != first.DownloadDir || got.TotalCount != first.TotalCount || got.PresentCount != first.PresentCount || got.MissingCount != first.MissingCount {
		t.Fatalf("snapshot mismatch: got %+v want %+v", got, first)
	}

	second := DownloadMirrorSnapshot{
		DownloadDir:  first.DownloadDir,
		ScannedAt:    first.ScannedAt.Add(5 * time.Minute),
		TotalCount:   11,
		PresentCount: 8,
		MissingCount: 3,
	}
	if err := d.UpsertDownloadMirrorSnapshot(ctx, second); err != nil {
		t.Fatalf("update snapshot: %v", err)
	}
	got, found, err = d.GetDownloadMirrorSnapshot(ctx, first.DownloadDir)
	if err != nil {
		t.Fatalf("get updated snapshot: %v", err)
	}
	if !found {
		t.Fatal("updated snapshot missing")
	}
	if !got.ScannedAt.Equal(second.ScannedAt) || got.TotalCount != second.TotalCount || got.PresentCount != second.PresentCount || got.MissingCount != second.MissingCount {
		t.Fatalf("updated snapshot mismatch: got %+v want %+v", got, second)
	}
}
