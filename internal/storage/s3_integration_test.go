package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// TestS3MinIOIntegration spins up against the docker-compose MinIO at
// http://localhost:9000 with the default minioadmin creds and the
// aws-backup-dev bucket. Skipped automatically when MinIO isn't reachable.
//
// Run manually: `make dev-up && go test ./internal/storage/ -run MinIO -v`
func TestS3MinIOIntegration(t *testing.T) {
	endpoint := envOr("AWS_BACKUP_TEST_S3_ENDPOINT", "http://localhost:9000")
	bucket := envOr("AWS_BACKUP_TEST_S3_BUCKET", "aws-backup-dev")

	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	if i := strings.Index(host, "/"); i > 0 {
		host = host[:i]
	}
	conn, err := net.DialTimeout("tcp", host, 300*time.Millisecond)
	if err != nil {
		t.Skipf("skipping: MinIO not reachable at %s (%v)", endpoint, err)
	}
	conn.Close()

	ctx := context.Background()
	s, err := NewS3Storage(ctx, S3Config{
		Endpoint:        endpoint,
		UsePathStyle:    true,
		Region:          "us-east-1",
		Bucket:          bucket,
		AccessKeyID:     envOr("AWS_BACKUP_TEST_S3_KEY", "minioadmin"),
		SecretAccessKey: envOr("AWS_BACKUP_TEST_S3_SECRET", "minioadmin"),
		StorageClass:    "STANDARD", // MinIO ignores deep-archive, use STANDARD in tests
	})
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	defer s.Close()

	// 5 MB random payload — larger than the single-part cutoff to exercise
	// the multipart path.
	const size = 5 * 1024 * 1024
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	wantHex := hex.EncodeToString(sum[:])

	key := fmt.Sprintf("integration-tests/%d.bin", time.Now().UnixNano())
	res, err := s.Put(ctx, key, bytes.NewReader(body), int64(size))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Logf("put etag=%s sha256=%s", res.ETag, res.ChecksumSHA256)

	h, err := s.Head(ctx, key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if h.Size != int64(size) {
		t.Errorf("head size=%d want %d", h.Size, size)
	}

	// The SHA256 checksum echoed by S3 should match what we computed
	// locally when the backend honors ChecksumAlgorithm.
	if res.ChecksumSHA256 != "" && res.ChecksumSHA256 != wantHex {
		t.Errorf("checksum mismatch:\n got:  %s\n want: %s", res.ChecksumSHA256, wantHex)
	}
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
