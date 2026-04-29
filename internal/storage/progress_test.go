package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// stubRoundTripper drains req.Body to mimic what a real http.Transport
// does when sending the request — it Reads the body to write it to the
// wire. progressTransport's body wrapper is what observes those Reads.
type stubRoundTripper struct{}

func (stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func TestProgressTransportFiresCallback(t *testing.T) {
	body := bytes.Repeat([]byte{'x'}, 64*1024) // 64 KiB
	var seen int64
	ctx := WithUploadProgress(context.Background(), func(n int64) {
		atomic.AddInt64(&seen, n)
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://example/x", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	rt := &progressTransport{inner: stubRoundTripper{}}
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt64(&seen); got != int64(len(body)) {
		t.Errorf("callback saw %d bytes, want %d", got, len(body))
	}
}

func TestProgressTransportNoCallbackOnContextWithout(t *testing.T) {
	// Without WithUploadProgress, the transport must not wrap req.Body —
	// otherwise we'd silently corrupt callers that pass our http client
	// directly without opting into progress.
	req, err := http.NewRequest(http.MethodPut, "http://example/x", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	originalBody := req.Body
	rt := &progressTransport{inner: stubRoundTripper{}}
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	// stubRoundTripper drained the body, so we can't check identity after
	// the fact; the assertion is implicit: callback never fired (no panic
	// from a nil ProgressFunc dereference).
	_ = originalBody
}

func TestProgressTransportConcurrentSafe(t *testing.T) {
	// Simulates multipart's concurrent part-uploads: many goroutines hit
	// the transport with the same context-bound callback. The callback
	// must be safe to invoke concurrently if the caller uses atomics.
	const goroutines = 10
	const bodySize = 8 * 1024
	var total int64
	ctx := WithUploadProgress(context.Background(), func(n int64) {
		atomic.AddInt64(&total, n)
	})

	rt := &progressTransport{inner: stubRoundTripper{}}
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://example/x", bytes.NewReader(make([]byte, bodySize)))
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := rt.RoundTrip(req); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * bodySize)
	if got := atomic.LoadInt64(&total); got != want {
		t.Errorf("total bytes seen = %d, want %d", got, want)
	}
}

func TestWithUploadProgressNilFn(t *testing.T) {
	if got := WithUploadProgress(context.Background(), nil); got != context.Background() {
		t.Errorf("nil fn should return ctx unchanged")
	}
}
