package storage

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestNewS3StorageMultipartThresholdDefault verifies the constructor's
// behaviour around the configurable cutoff: zero or negative input falls
// back to the 5 GiB hard ceiling; positive input is stored verbatim.
// Validates against a fake (no-credentials) bucket — NewS3Storage doesn't
// hit the network during construction.
func TestNewS3StorageMultipartThresholdDefault(t *testing.T) {
	ctx := context.Background()
	base := S3Config{
		Region:          "us-east-1",
		Bucket:          "test",
		AccessKeyID:     "x",
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

func TestS3StorageRestoreUsesRequestedTier(t *testing.T) {
	ctx := context.Background()
	var (
		mu      sync.Mutex
		seen    []string
		readErr error
	)

	s, err := NewS3Storage(ctx, S3Config{
		Region:          "us-east-1",
		Bucket:          "test",
		AccessKeyID:     "x",
		SecretAccessKey: "y",
	})
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("x", "y", "")),
	)
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	s.client = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = nil
		o.UsePathStyle = true
		o.HTTPClient = &http.Client{
			Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				b, err := io.ReadAll(req.Body)
				if err != nil {
					mu.Lock()
					readErr = err
					mu.Unlock()
					return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
				}
				mu.Lock()
				seen = append(seen, string(b))
				mu.Unlock()
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("{}")),
					Header:     make(http.Header),
				}, nil
			}),
		}
	})

	for _, tc := range []struct {
		name string
		tier RestoreTier
		want string
	}{
		{name: "bulk", tier: RestoreTierBulk, want: "Bulk"},
		{name: "standard", tier: RestoreTierStandard, want: "Standard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			seen = nil
			readErr = nil
			mu.Unlock()
			if err := s.Restore(ctx, "folder/object.bin", 7, tc.tier); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if readErr != nil {
				t.Fatalf("read body: %v", readErr)
			}
			if len(seen) != 1 {
				t.Fatalf("requests = %d want 1", len(seen))
			}
			if !strings.Contains(seen[0], tc.want) {
				t.Fatalf("restore body %q does not contain %q", seen[0], tc.want)
			}
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
