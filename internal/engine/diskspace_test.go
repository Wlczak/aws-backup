package engine

import (
	"errors"
	"strings"
	"testing"
)

func TestAvailableBytes_Positive(t *testing.T) {
	n, err := availableBytes(t.TempDir())
	if err != nil {
		t.Fatalf("availableBytes: %v", err)
	}
	if n == 0 {
		t.Fatal("availableBytes returned 0 — expected a positive value for the test tmp dir")
	}
}

func stubDiskAvailable(t *testing.T, fn func(string) (uint64, error)) {
	t.Helper()
	prev := diskAvailable
	diskAvailable = fn
	t.Cleanup(func() { diskAvailable = prev })
}

func TestEnsureTmpSpace_OK(t *testing.T) {
	stubDiskAvailable(t, func(string) (uint64, error) { return 1 << 30, nil })
	if err := ensureTmpSpace(t.TempDir(), 100<<20); err != nil {
		t.Fatalf("ensureTmpSpace: %v", err)
	}
}

func TestEnsureTmpSpace_TooSmall(t *testing.T) {
	stubDiskAvailable(t, func(string) (uint64, error) { return 50 << 20, nil })
	err := ensureTmpSpace(t.TempDir(), 100<<20)
	if err == nil {
		t.Fatal("expected error for insufficient space, got nil")
	}
	if !strings.Contains(err.Error(), "not enough space") {
		t.Fatalf("error %q should mention 'not enough space'", err)
	}
}

func TestEnsureTmpSpace_StatfsErrorIsNonFatal(t *testing.T) {
	stubDiskAvailable(t, func(string) (uint64, error) { return 0, errors.New("statfs blew up") })
	if err := ensureTmpSpace(t.TempDir(), 100<<20); err != nil {
		t.Fatalf("statfs failure should not block the run, got: %v", err)
	}
}
