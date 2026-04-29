package storage

import (
	"context"
	"io"
	"net/http"
)

// ProgressFunc is invoked with the byte count of every Read the HTTP
// transport makes off a request body — i.e. bytes flushed to the wire,
// not bytes the SDK has buffered internally. Multipart uploads invoke
// it concurrently from per-part goroutines; callers must synchronize
// any shared state (e.g. via atomic counters).
type ProgressFunc func(bytesSent int64)

type progressKey struct{}

// WithUploadProgress attaches fn to ctx so the transport installed by
// NewS3Storage will tick it for every Read off the request body. nil fn
// returns ctx unchanged. Scope the callback to a single upload — the
// transport sees every request that flows through this S3 client. (#)
func WithUploadProgress(ctx context.Context, fn ProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressKey{}, fn)
}

// progressTransport wraps another RoundTripper and, if the request's
// context carries a ProgressFunc, replaces req.Body with a counting
// wrapper that ticks the callback on every Read. Reads happen at the
// http.Transport's natural framing granularity (typically 16-32 KiB),
// so progress reflects bytes actually being written to the TCP socket.
type progressTransport struct {
	inner http.RoundTripper
}

func (t *progressTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if fn, _ := req.Context().Value(progressKey{}).(ProgressFunc); fn != nil && req.Body != nil {
		req.Body = &countingReadCloser{rc: req.Body, fn: fn}
	}
	return t.inner.RoundTrip(req)
}

type countingReadCloser struct {
	rc io.ReadCloser
	fn ProgressFunc
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.fn(int64(n))
	}
	return n, err
}

func (c *countingReadCloser) Close() error { return c.rc.Close() }

// countingReader is the plain io.Reader variant used by MemStorage to
// mirror the http transport's behaviour without needing a Closer.
type countingReader struct {
	r  io.Reader
	fn ProgressFunc
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.fn(int64(n))
	}
	return n, err
}
