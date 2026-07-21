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

func TestDefaultScheduleDisabled(t *testing.T) {
	if got := Default().Backup.Schedule; got != "" {
		t.Errorf("Default().Backup.Schedule = %q; want empty (scheduled runs disabled by default)", got)
	}
}

func TestStarterProfileRequiresOnboardingWithoutDevCredentials(t *testing.T) {
	central := DefaultCentral()
	if !central.SetupRequired() {
		t.Fatal("fresh central config should require setup")
	}
	profile := StarterProfile()
	cfg := profile.ToConfig(central.Server)
	if cfg.Source.Type != "" || cfg.S3.Bucket != "" || cfg.S3.Endpoint != "" {
		t.Fatalf("starter unexpectedly configured: source=%q bucket=%q endpoint=%q", cfg.Source.Type, cfg.S3.Bucket, cfg.S3.Endpoint)
	}
	if cfg.S3.AccessKeyID != "" || cfg.S3.SecretAccessKey != "" {
		t.Fatal("starter contains credentials")
	}
	if err := cfg.ValidateForSetup(); err != nil {
		t.Fatalf("starter bootstrap validation: %v", err)
	}
}

func TestLegacyCentralWithPasswordIsGrandfathered(t *testing.T) {
	central := CentralConfig{
		ActiveProfile: "default",
		Server:        Default().Server,
		Auth:          AuthConfig{PasswordHash: "present"},
	}
	if central.SetupRequired() {
		t.Fatal("legacy configured install should be grandfathered")
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
		{"missing region with bucket", func(c *Config) { c.S3.Region = "" }, "s3.region is required"},
		{"bad chunk size", func(c *Config) { c.Backup.ChunkSize = 0 }, "chunk_size"},
		{"bad scan batch bytes", func(c *Config) { c.Backup.ScanBatchBytes = 0 }, "scan_batch_bytes"},
		{"bad cron", func(c *Config) { c.Backup.Schedule = "definitely not cron" }, "schedule invalid"},
		{"bad port", func(c *Config) { c.Server.Port = 70000 }, "server.port"},
		{"negative multipart threshold", func(c *Config) { c.S3.MultipartThreshold = -1 }, "multipart_threshold"},
		{"multipart threshold over 5 GiB", func(c *Config) { c.S3.MultipartThreshold = 5*1024*1024*1024 + 1 }, "5 GiB single-PutObject ceiling"},
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

func TestValidateAllowsUnconfiguredS3(t *testing.T) {
	cfg := Default()
	cfg.Source.LocalDir.Root = "/tmp/x"
	cfg.S3.Bucket = ""
	cfg.S3.Region = ""
	cfg.S3.StorageClass = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config without S3 bucket should validate: %v", err)
	}
}

func TestValidateMultipartThresholdAccepts(t *testing.T) {
	for _, v := range []int64{0, 16 * 1024 * 1024, 5 * 1024 * 1024 * 1024} {
		cfg := Default()
		cfg.Source.LocalDir.Root = "/tmp/x"
		cfg.S3.MultipartThreshold = v
		if err := cfg.Validate(); err != nil {
			t.Errorf("threshold=%d: unexpected error %v", v, err)
		}
	}
}

func TestValidateGlacierClassRequiresAWS(t *testing.T) {
	// Custom endpoint + Glacier-tier class is rejected because S3-compatible
	// services don't implement those tiers.
	for _, class := range []string{StorageClassDeepArchive, StorageClassGlacier, StorageClassGlacierIR} {
		t.Run("reject_"+class+"_with_endpoint", func(t *testing.T) {
			cfg := Default()
			cfg.Source.LocalDir.Root = "/tmp/x"
			cfg.S3.Endpoint = "http://minio:9000"
			cfg.S3.StorageClass = class
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "requires AWS S3") {
				t.Fatalf("expected 'requires AWS S3' error for class %q, got %v", class, err)
			}
		})
	}

	// Empty endpoint (real AWS) accepts every storage class.
	for _, class := range []string{StorageClassDeepArchive, StorageClassGlacier, StorageClassGlacierIR, StorageClassStandard} {
		t.Run("accept_"+class+"_on_aws", func(t *testing.T) {
			cfg := Default()
			cfg.Source.LocalDir.Root = "/tmp/x"
			cfg.S3.Endpoint = ""
			cfg.S3.StorageClass = class
			if err := cfg.Validate(); err != nil {
				t.Fatalf("class %q on real AWS should validate, got %v", class, err)
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

func TestProfilePathsAndValidation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config.json")
	for _, name := range []string{"default", "photos-2026", "a.b_c"} {
		if err := ValidateProfileName(name); err != nil {
			t.Fatalf("ValidateProfileName(%q): %v", name, err)
		}
		p, err := ProfilePath(root, name)
		if err != nil {
			t.Fatalf("ProfilePath(%q): %v", name, err)
		}
		if !strings.Contains(p, filepath.Join("profiles", name, "config.json")) {
			t.Fatalf("profile path %q does not include expected layout", p)
		}
	}
	for _, name := range []string{"", "../x", "x/y", ".hidden", "..", strings.Repeat("a", 65)} {
		if err := ValidateProfileName(name); err == nil {
			t.Fatalf("ValidateProfileName(%q) succeeded; want error", name)
		}
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected error loading missing file")
	}
}
