package api

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type cacheTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *cacheTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *cacheTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestResponseCacheHitRevisionAndExpiry(t *testing.T) {
	clock := &cacheTestClock{now: time.Unix(100, 0)}
	cache := newResponseCache(1<<20, 10, 5*time.Minute, 500*time.Millisecond, clock.Now)
	var revision atomic.Uint64
	var calls atomic.Int64
	load := func(context.Context) ([]byte, error) {
		return []byte{byte(calls.Add(1))}, nil
	}

	first, err := cache.Get(context.Background(), "stats", revision.Load, load)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Get(context.Background(), "stats", revision.Load, load)
	if err != nil {
		t.Fatal(err)
	}
	if first[0] != 1 || second[0] != 1 || calls.Load() != 1 {
		t.Fatalf("first=%v second=%v calls=%d", first, second, calls.Load())
	}

	revision.Add(1)
	third, err := cache.Get(context.Background(), "stats", revision.Load, load)
	if err != nil {
		t.Fatal(err)
	}
	if third[0] != 2 || calls.Load() != 2 {
		t.Fatalf("revision miss body=%v calls=%d", third, calls.Load())
	}

	clock.Advance(5*time.Minute + time.Nanosecond)
	fourth, err := cache.Get(context.Background(), "stats", revision.Load, load)
	if err != nil {
		t.Fatal(err)
	}
	if fourth[0] != 3 || calls.Load() != 3 {
		t.Fatalf("expiry miss body=%v calls=%d", fourth, calls.Load())
	}
}

func TestResponseCacheDoesNotStoreAcrossRevisionChange(t *testing.T) {
	cache := newResponseCache(1<<20, 10, time.Minute, time.Second, time.Now)
	var revision atomic.Uint64
	var calls atomic.Int64
	load := func(context.Context) ([]byte, error) {
		calls.Add(1)
		revision.Add(1)
		return []byte("value"), nil
	}

	_, _ = cache.Get(context.Background(), "stats", revision.Load, load)
	_, _ = cache.Get(context.Background(), "stats", revision.Load, load)
	if calls.Load() != 2 {
		t.Fatalf("loader calls=%d want 2", calls.Load())
	}
}

func TestResponseCacheErrorBackoffAndCancellation(t *testing.T) {
	clock := &cacheTestClock{now: time.Unix(100, 0)}
	cache := newResponseCache(1<<20, 10, 5*time.Minute, 500*time.Millisecond, clock.Now)
	var revision atomic.Uint64
	var calls atomic.Int64
	wantErr := errors.New("database unavailable")
	loadErr := func(context.Context) ([]byte, error) {
		calls.Add(1)
		return nil, wantErr
	}

	for range 2 {
		if _, err := cache.Get(context.Background(), "stats", revision.Load, loadErr); !errors.Is(err, wantErr) {
			t.Fatalf("err=%v want %v", err, wantErr)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("cached error calls=%d want 1", calls.Load())
	}
	clock.Advance(500*time.Millisecond + time.Nanosecond)
	_, _ = cache.Get(context.Background(), "stats", revision.Load, loadErr)
	if calls.Load() != 2 {
		t.Fatalf("expired error calls=%d want 2", calls.Load())
	}

	cancelCalls := 0
	loadCanceled := func(context.Context) ([]byte, error) {
		cancelCalls++
		return nil, context.Canceled
	}
	for range 2 {
		_, _ = cache.Get(context.Background(), "cancel", revision.Load, loadCanceled)
	}
	if cancelCalls != 2 {
		t.Fatalf("cancel calls=%d want 2", cancelCalls)
	}
}

func TestResponseCacheSingleflight(t *testing.T) {
	cache := newResponseCache(1<<20, 10, time.Minute, time.Second, time.Now)
	var revision atomic.Uint64
	var calls atomic.Int64
	release := make(chan struct{})
	started := make(chan struct{})
	load := func(context.Context) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return []byte("shared"), nil
	}

	const workers = 20
	var wg sync.WaitGroup
	wg.Add(workers)
	errs := make(chan error, workers)
	for range workers {
		go func() {
			defer wg.Done()
			body, err := cache.Get(context.Background(), "tree", revision.Load, load)
			if err != nil {
				errs <- err
				return
			}
			if string(body) != "shared" {
				errs <- errors.New("unexpected body")
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("loader calls=%d want 1", calls.Load())
	}
}

func TestResponseCacheLRUAndOversizedBypass(t *testing.T) {
	clock := &cacheTestClock{now: time.Unix(100, 0)}
	cache := newResponseCache(310, 10, time.Minute, time.Second, clock.Now)
	var revision atomic.Uint64
	counts := map[string]int{}
	load := func(key string, size int) func(context.Context) ([]byte, error) {
		return func(context.Context) ([]byte, error) {
			counts[key]++
			return make([]byte, size), nil
		}
	}

	_, _ = cache.Get(context.Background(), "a", revision.Load, load("a", 20))
	_, _ = cache.Get(context.Background(), "b", revision.Load, load("b", 20))
	_, _ = cache.Get(context.Background(), "a", revision.Load, load("a", 20))
	_, _ = cache.Get(context.Background(), "c", revision.Load, load("c", 20))
	_, _ = cache.Get(context.Background(), "b", revision.Load, load("b", 20))
	if counts["a"] != 1 || counts["b"] != 2 || counts["c"] != 1 {
		t.Fatalf("LRU loader counts=%v", counts)
	}

	_, _ = cache.Get(context.Background(), "huge", revision.Load, load("huge", 400))
	_, _ = cache.Get(context.Background(), "huge", revision.Load, load("huge", 400))
	if counts["huge"] != 2 {
		t.Fatalf("oversized loader calls=%d want 2", counts["huge"])
	}
}

func TestResponseCacheClearRejectsInflightStore(t *testing.T) {
	cache := newResponseCache(1<<20, 10, time.Minute, time.Second, time.Now)
	var revision atomic.Uint64
	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	load := func(context.Context) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return []byte("value"), nil
	}

	done := make(chan struct{})
	go func() {
		_, _ = cache.Get(context.Background(), "files", revision.Load, load)
		close(done)
	}()
	<-started
	cache.Clear()
	close(release)
	<-done
	_, _ = cache.Get(context.Background(), "files", revision.Load, load)
	if calls.Load() != 2 {
		t.Fatalf("loader calls=%d want 2", calls.Load())
	}
}
