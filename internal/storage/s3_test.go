package storage

import (
	"context"
	"testing"
)

// TestNewS3StorageMultipartThresholdDefault verifies the constructor's
// behaviour around the configurable cutoff: zero or negative input falls
// back to the 5 GiB hard ceiling; positive input is stored verbatim.
// Validates against a fake (no-credentials) bucket — NewS3Storage doesn't
// hit the network during construction.
func TestNewS3StorageMultipartThresholdDefault(t *testing.T) {
	ctx := context.Background()
	base := S3Config{
		Region:      "us-east-1",
		Bucket:      "test",
		AccessKeyID: "x",
		SecretAccessKey: "y",
	}

	for _, tc := range []struct {
		name string
		in   int64
		want int64
	}{
		{"zero defaults to 5 GiB", 0, defaultMultipartThreshold},
		{"negative defaults to 5 GiB", -1, defaultMultipartThreshold},
		{"positive stored verbatim", 16 << 20, 16 << 20},
		{"5 GiB stored verbatim", 5 * 1024 * 1024 * 1024, 5 * 1024 * 1024 * 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.MultipartThreshold = tc.in
			s, err := NewS3Storage(ctx, cfg)
			if err != nil {
				t.Fatalf("NewS3Storage: %v", err)
			}
			if s.multipartThreshold != tc.want {
				t.Errorf("multipartThreshold = %d, want %d", s.multipartThreshold, tc.want)
			}
		})
	}
}
