package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewRequiresTrigger(t *testing.T) {
	if _, err := New("0 2 * * *", nil, nil); err == nil {
		t.Fatal("expected error for nil trigger")
	}
}

func TestNewRejectsBadExpr(t *testing.T) {
	if _, err := New("not a cron", func(context.Context) error { return nil }, quietLogger()); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestSchedulerFires(t *testing.T) {
	// robfig/cron ticks once per second so "@every 1s" is the fastest
	// realistic interval.
	var calls atomic.Int64
	s, err := New("@every 1s", func(context.Context) error {
		calls.Add(1)
		return nil
	}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Stop()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("expected >=2 fires, got %d", calls.Load())
}

func TestSchedulerUpdate(t *testing.T) {
	var calls atomic.Int64
	s, err := New("", func(context.Context) error { calls.Add(1); return nil }, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Stop()

	time.Sleep(500 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("empty schedule should not fire: got %d", calls.Load())
	}

	if err := s.Update("@every 1s"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatalf("want >=2 after update, got %d", calls.Load())
	}

	// Disable by updating to empty — no further fires after a settle window.
	if err := s.Update(""); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	time.Sleep(1500 * time.Millisecond)
	after := calls.Load()
	if after != before {
		t.Fatalf("schedule not disabled: before=%d after=%d", before, after)
	}
}

func TestSchedulerUpdateRejectsBadExpr(t *testing.T) {
	s, _ := New("", func(context.Context) error { return nil }, quietLogger())
	if err := s.Update("gibberish"); err == nil {
		t.Fatal("expected error")
	}
}

func TestSchedulerLogsTriggerError(t *testing.T) {
	// Capture log output to verify the warning.
	var sink testSink
	handler := slog.NewTextHandler(&sink, nil)
	logger := slog.New(handler)

	s, err := New("@every 1s", func(context.Context) error {
		return errors.New("kaboom")
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	defer s.Stop()

	time.Sleep(1500 * time.Millisecond)
	if !sink.contains("kaboom") {
		t.Fatalf("expected warning log, got: %s", sink.b)
	}
}

func TestNextReportsFireTime(t *testing.T) {
	s, _ := New("@every 1s", func(context.Context) error { return nil }, quietLogger())
	s.Start()
	defer s.Stop()
	if s.Next().IsZero() {
		t.Fatal("Next() should be non-zero after Start")
	}
}

type testSink struct {
	mu sync.Mutex
	b  []byte
}

func (s *testSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.b = append(s.b, p...)
	s.mu.Unlock()
	return len(p), nil
}
func (s *testSink) contains(sub string) bool {
	s.mu.Lock()
	str := string(s.b)
	s.mu.Unlock()
	return contains(str, sub)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()))
}
