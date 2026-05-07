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
	fn, _ := req.Context().Value(progressKey{}).(ProgressFunc)
	if fn == nil || req.Body == nil {
		return t.inner.RoundTrip(req)
	}
	wrapper := &countingReadCloser{rc: req.Body, fn: fn}
	req.Body = wrapper
	// Preserve retryability: the SDK rewinds a body on retry by calling
	// req.GetBody() (sigv4 retries) or by Seek-to-0 (redirects). The
	// previous wrapper masked both — retries either lost progress
	// reporting entirely (GetBody returned the unwrapped body) or saw
	// cumulative byte counts overshoot the file size on subsequent
	// attempts. Wrap GetBody so retries stay counted, and refund the
	// previous attempt's bytes so the consumer's running total stays
	// honest. (#168)
	if req.GetBody != nil {
		orig := req.GetBody
		req.GetBody = func() (io.ReadCloser, error) {
			rc, err := orig()
			if err != nil {
				return nil, err
			}
			if wrapper.n > 0 {
				fn(-wrapper.n)
				wrapper.n = 0
			}
			wrapper.rc = rc
			return wrapper, nil
		}
	}
	return t.inner.RoundTrip(req)
}

type countingReadCloser struct {
	rc io.ReadCloser
	fn ProgressFunc
	n  int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.n += int64(n)
		c.fn(int64(n))
	}
	return n, err
}

// Seek forwards to the underlying body when it supports io.Seeker (most
// upload bodies are *os.File or *bytes.Reader, both of which do). On a
// rewind to offset 0 — the SDK's retry signal — refund the bytes
// counted on the previous attempt so the consumer's running progress
// total doesn't overshoot. (#168)
func (c *countingReadCloser) Seek(offset int64, whence int) (int64, error) {
	sk, ok := c.rc.(io.Seeker)
	if !ok {
		return 0, io.ErrUnexpectedEOF
	}
	pos, err := sk.Seek(offset, whence)
	if err == nil && pos == 0 && c.n > 0 {
		c.fn(-c.n)
		c.n = 0
	}
	return pos, err
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
