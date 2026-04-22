package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func TestMemStorageRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := NewMemStorage()

	body := []byte("hello world, hello world, hello world")
	sum := sha256.Sum256(body)
	wantHex := hex.EncodeToString(sum[:])

	res, err := m.Put(ctx, "backups/x.zip", bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != int64(len(body)) {
		t.Errorf("size=%d want %d", res.Size, len(body))
	}
	if res.ChecksumSHA256 != wantHex {
		t.Errorf("sha256 mismatch: got %s want %s", res.ChecksumSHA256, wantHex)
	}

	h, err := m.Head(ctx, "backups/x.zip")
	if err != nil {
		t.Fatal(err)
	}
	if h.Size != int64(len(body)) {
		t.Errorf("head size=%d", h.Size)
	}

	got, ok := m.Get("backups/x.zip")
	if !ok {
		t.Fatal("Get: not found")
	}
	if !bytes.Equal(got, body) {
		t.Fatal("Get returned different bytes")
	}
}

func TestMemStorageMissing(t *testing.T) {
	ctx := context.Background()
	m := NewMemStorage()

	_, err := m.Head(ctx, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := m.Restore(ctx, "nope", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Restore: want ErrNotFound, got %v", err)
	}
}
