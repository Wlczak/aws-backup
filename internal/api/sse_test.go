package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/events"
)

func TestSSEDeliversEvents(t *testing.T) {
	bus := events.NewBus(8)
	srv := httptest.NewServer(sseHandler(bus, slog.Default()))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Wait briefly for the subscription to register (bus.Subscribe happens
	// inside the handler after headers are written).
	waitFor(t, func() bool { return bus.SubscriberCount() == 1 }, 500*time.Millisecond)

	bus.Publish(engine.Event{Type: engine.EventUploadComplete, RunID: 42,
		Data: map[string]any{"key": "backups/x.zip"}})

	// Parse the SSE stream.
	events, err := readNextEvent(resp.Body, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if events.event != "upload_complete" {
		t.Errorf("event=%q want upload_complete", events.event)
	}
	var ev engine.Event
	if err := json.Unmarshal([]byte(events.data), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.RunID != 42 {
		t.Errorf("run_id=%d want 42", ev.RunID)
	}
}

func TestSSECleansUpOnDisconnect(t *testing.T) {
	bus := events.NewBus(8)
	srv := httptest.NewServer(sseHandler(bus, slog.Default()))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return bus.SubscriberCount() == 1 }, 500*time.Millisecond)

	resp.Body.Close()

	// The server-side handler should notice the disconnect and unsubscribe.
	waitFor(t, func() bool { return bus.SubscriberCount() == 0 }, time.Second)
	if bus.SubscriberCount() != 0 {
		t.Errorf("subscribers=%d want 0", bus.SubscriberCount())
	}
}

type sseFrame struct{ event, data string }

func readNextEvent(r io.Reader, timeout time.Duration) (sseFrame, error) {
	type result struct {
		f   sseFrame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var f sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			case line == "" && f.event != "":
				ch <- result{f: f}
				return
			}
		}
		ch <- result{err: sc.Err()}
	}()
	select {
	case r := <-ch:
		return r.f, r.err
	case <-time.After(timeout):
		return sseFrame{}, io.ErrUnexpectedEOF
	}
}

// TestSSEMarshalErrorLogsAndClosesStream verifies that an event with an
// unmarshallable payload is logged at error level and causes the stream
// to close (so EventSource auto-reconnects) rather than silently
// continuing. (#72)
func TestSSEMarshalErrorLogsAndClosesStream(t *testing.T) {
	bus := events.NewBus(8)
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := httptest.NewServer(sseHandler(bus, log))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	waitFor(t, func() bool { return bus.SubscriberCount() == 1 }, 500*time.Millisecond)

	// Channels are not JSON-serialisable; this event must fail to marshal.
	bus.Publish(engine.Event{
		Type:  "upload_complete",
		RunID: 7,
		Data:  map[string]any{"bad": make(chan int)},
	})

	// The handler should close the stream after the marshal error.
	waitFor(t, func() bool { return bus.SubscriberCount() == 0 }, time.Second)
	if bus.SubscriberCount() != 0 {
		t.Errorf("subscribers=%d want 0 after marshal error", bus.SubscriberCount())
	}

	// The error must have been logged with the event type.
	logged := logBuf.String()
	if !strings.Contains(logged, "upload_complete") {
		t.Errorf("expected event_type in log, got: %s", logged)
	}
	if !strings.Contains(logged, "sse: failed to marshal event") {
		t.Errorf("expected error message in log, got: %s", logged)
	}
}

func waitFor(t *testing.T, pred func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
