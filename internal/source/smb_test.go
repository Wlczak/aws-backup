package source

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestSMBIntegration runs against a real SMB server if one is configured
// via env vars. Skipped otherwise so CI / dev machines without a share
// can still run `go test ./...` green.
//
// Env vars:
//
//	AWS_BACKUP_TEST_SMB_HOST       (required to not skip)
//	AWS_BACKUP_TEST_SMB_PORT       (default 445)
//	AWS_BACKUP_TEST_SMB_USER
//	AWS_BACKUP_TEST_SMB_PASS
//	AWS_BACKUP_TEST_SMB_DOMAIN
//	AWS_BACKUP_TEST_SMB_SHARE      (required to not skip)
//	AWS_BACKUP_TEST_SMB_PATH       (optional sub-path, may be empty)
func TestSMBIntegration(t *testing.T) {
	host := os.Getenv("AWS_BACKUP_TEST_SMB_HOST")
	share := os.Getenv("AWS_BACKUP_TEST_SMB_SHARE")
	if host == "" || share == "" {
		t.Skipf("skipping: AWS_BACKUP_TEST_SMB_HOST / _SHARE not set")
	}

	port := 445
	if p := os.Getenv("AWS_BACKUP_TEST_SMB_PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}

	src, err := NewSMB(SMBConfig{
		Host:     host,
		Port:     port,
		Username: os.Getenv("AWS_BACKUP_TEST_SMB_USER"),
		Password: os.Getenv("AWS_BACKUP_TEST_SMB_PASS"),
		Domain:   os.Getenv("AWS_BACKUP_TEST_SMB_DOMAIN"),
		Share:    share,
		Path:     os.Getenv("AWS_BACKUP_TEST_SMB_PATH"),
	})
	if err != nil {
		t.Fatalf("NewSMB: %v", err)
	}
	defer src.Close()

	count := 0
	err = src.Walk(context.Background(), func(e Entry) error {
		if e.IsDir {
			return nil
		}
		count++
		if count == 1 {
			rc, err := src.Open(context.Background(), e.RelPath)
			if err != nil {
				return err
			}
			defer rc.Close()
			// Read a few bytes to verify the stream works.
			buf := make([]byte, 16)
			if _, err := rc.Read(buf); err != nil && err != io.EOF {
				return err
			}
		}
		if count >= 5 {
			return io.EOF // stop early
		}
		return nil
	})
	if err != nil && err != io.EOF {
		t.Fatalf("Walk: %v", err)
	}
	if count == 0 {
		t.Error("walked zero files — share empty or path wrong?")
	}
	t.Logf("walked %d files from smb://%s/%s%s", count, host, share, os.Getenv("AWS_BACKUP_TEST_SMB_PATH"))
}

func TestSMBValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  SMBConfig
	}{
		{"missing host", SMBConfig{Share: "s"}},
		{"missing share", SMBConfig{Host: "h"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSMB(tc.cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestSMBOpenPathIsShareRelative(t *testing.T) {
	tests := []struct {
		name string
		root string
		rel  string
		want string
	}{
		{
			name: "share root with slash path",
			rel:  "Photos 2023/IMG_9273.JPG",
			want: "Photos 2023/IMG_9273.JPG",
		},
		{
			name: "share root with leading slash",
			rel:  "/Photos 2023/IMG_9273.JPG",
			want: "Photos 2023/IMG_9273.JPG",
		},
		{
			name: "windows separators",
			root: `\Backups\Photos\`,
			rel:  `2023\IMG_9273.JPG`,
			want: "Backups/Photos/2023/IMG_9273.JPG",
		},
		{
			name: "traversal remains under configured root",
			root: "Backups/Photos",
			rel:  "../../IMG_9273.JPG",
			want: "Backups/Photos/IMG_9273.JPG",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &SMB{cfg: SMBConfig{Path: tt.root}}
			got, err := src.openPath(tt.rel)
			if err != nil {
				t.Fatalf("openPath: %v", err)
			}
			if got != tt.want {
				t.Fatalf("openPath() = %q, want %q", got, tt.want)
			}
			if strings.HasPrefix(got, "/") || strings.HasPrefix(got, `\`) {
				t.Fatalf("openPath() returned an absolute SMB path: %q", got)
			}
		})
	}
}
