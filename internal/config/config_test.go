package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultValidates(t *testing.T) {
	cfg := Default()
	cfg.Source.LocalDir.Root = t.TempDir()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default config should validate after setting localdir.root: %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.json")

	want := Default()
	want.Source.LocalDir.Root = "/data"
	want.S3.Bucket = "my-bucket"

	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("round trip mismatch\n got:  %s\n want: %s", gotJSON, wantJSON)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"missing source type", func(c *Config) { c.Source.Type = "" }, "source.type is required"},
		{"bad source type", func(c *Config) { c.Source.Type = "ftp" }, "invalid"},
		{"localdir without root", func(c *Config) { c.Source.LocalDir.Root = "" }, "localdir.root is required"},
		{"smb without host", func(c *Config) {
			c.Source.Type = SourceSMB
			c.Source.SMB.Host = ""
			c.Source.SMB.Share = "s"
			c.Source.SMB.Port = 445
		}, "smb.host is required"},
		{"smb bad port", func(c *Config) {
			c.Source.Type = SourceSMB
			c.Source.SMB.Host = "x"
			c.Source.SMB.Share = "s"
			c.Source.SMB.Port = 0
		}, "smb.port"},
		{"missing bucket", func(c *Config) { c.S3.Bucket = "" }, "s3.bucket is required"},
		{"bad chunk size", func(c *Config) { c.Backup.ChunkSize = 0 }, "chunk_size"},
		{"bad cron", func(c *Config) { c.Backup.Schedule = "definitely not cron" }, "schedule invalid"},
		{"bad port", func(c *Config) { c.Server.Port = 70000 }, "server.port"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Source.LocalDir.Root = "/tmp/x"
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestRedacted(t *testing.T) {
	cfg := Default()
	cfg.Source.SMB.Password = "hunter2"
	cfg.S3.AccessKeyID = "AKIA..."
	cfg.S3.SecretAccessKey = "verysecret"

	r := cfg.Redacted()
	if r.Source.SMB.Password != RedactedMarker {
		t.Errorf("smb password not redacted: %q", r.Source.SMB.Password)
	}
	if r.S3.AccessKeyID != RedactedMarker {
		t.Errorf("access key id not redacted: %q", r.S3.AccessKeyID)
	}
	if r.S3.SecretAccessKey != RedactedMarker {
		t.Errorf("secret access key not redacted: %q", r.S3.SecretAccessKey)
	}
	// Original must be untouched.
	if cfg.S3.SecretAccessKey != "verysecret" {
		t.Errorf("Redacted() mutated the original")
	}
}

func TestDefaultPath(t *testing.T) {
	p, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if p == "" {
		t.Fatalf("DefaultPath returned empty")
	}
	if filepath.Base(p) != "config.json" {
		t.Errorf("expected filename config.json, got %s", p)
	}
	if !strings.Contains(p, appDirName) {
		t.Errorf("expected path to include %q, got %s", appDirName, p)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected error loading missing file")
	}
}
