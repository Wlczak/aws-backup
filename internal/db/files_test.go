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
