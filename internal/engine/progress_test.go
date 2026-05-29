package engine

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestProgressReader_CountsAllBytesAndEmitsFinal(t *testing.T) {
	const total = 4096
	src := bytes.NewReader(bytes.Repeat([]byte("a"), total))

	var samples []int64
	pr := newProgressReader(src, total, time.Hour, func(read, want int64) {
		if want != total {
			t.Errorf("total = %d, want %d", want, total)
		}
		samples = append(samples, read)
	})

	n, err := io.Copy(io.Discard, pr)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n != total {
		t.Errorf("copied %d bytes, want %d", n, total)
	}
	if pr.read != total {
		t.Errorf("pr.read = %d, want %d", pr.read, total)
	}
	if len(samples) == 0 {
		t.Fatal("expected at least one progress sample (final EOF)")
	}
	if got := samples[len(samples)-1]; got != total {
		t.Errorf("final sample = %d, want %d (EOF should always emit)", got, total)
	}
}

func TestProgressReader_Throttles(t *testing.T) {
	// Source large enough to require many small Reads.
	const total = 1 << 16
	src := bytes.NewReader(bytes.Repeat([]byte("x"), total))

	now := time.Unix(0, 0)
	var samples []int64
	pr := newProgressReader(src, total, 500*time.Millisecond, func(read, _ int64) {
		samples = append(samples, read)
	})
	pr.now = func() time.Time { return now }

	// Each Read advances simulated time by 100ms — under the 500ms interval.
	// We expect: one emit on the first Read (lastEmit zero), then no emits
	// until 500ms have elapsed, then one more, etc., plus a final EOF emit.
	buf := make([]byte, 1024)
	for {
		_, err := pr.Read(buf)
		now = now.Add(100 * time.Millisecond)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}

	if len(samples) < 2 {
		t.Fatalf("expected throttling to leave multiple samples, got %d", len(samples))
	}
	// 64 reads at 100ms each ≈ 6.4s; with 500ms throttle that's roughly
	// 13 emits + 1 final. Just check it's not "every read" (64+) and not
	// "only one".
	if len(samples) > 20 {
		t.Errorf("too many samples %d — throttling not effective", len(samples))
	}
	if got := samples[len(samples)-1]; got != int64(total) {
		t.Errorf("final sample = %d, want %d", got, total)
	}
}

func TestProgressReader_NilCallbackIsSafe(t *testing.T) {
	src := bytes.NewReader([]byte("hello"))
	pr := newProgressReader(src, 5, time.Second, nil)
	out, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("got %q, want hello", out)
	}
}
