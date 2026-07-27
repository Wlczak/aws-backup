// Package api hosts the HTTP layer: chi router, JSON handlers, and the
// Server-Sent Events (SSE) stream that feeds the dashboard.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/events"
)

// sseHandler returns an http.Handler that streams events from bus to the
// client over Server-Sent Events. The handler returns when the client
// disconnects (ctx.Done fires) or the subscription is closed.
//
// replay, when non-nil, is called once at connect time and its events
// are flushed to the client before the live stream begins. This lets
// a client that refreshes mid-run receive the full run history rather
// than starting blind. (#130)
//
// replay also returns a cutoff time (the moment captured before the
// replay query started). The live forwarder drops EventRunLog events
// whose At is at or before the cutoff to avoid double-emitting log
// rows that landed on the bus while ListLogs was running. The window
// is brief — once a live run_log with At > cutoff arrives, the cutoff
// is cleared so future events flow unconditionally. (#176)
//
// log is used to surface marshal errors.
func sseHandler(bus *events.Bus, log *slog.Logger, replay func(context.Context) ([]engine.Event, time.Time), shutdown <-chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		// Subscribe before writing headers so a 503 on subscriber-cap
		// overflow goes out as a real error response rather than after
		// a 200 has already been committed. (#236)
		sub := bus.Subscribe()
		if sub == nil {
			http.Error(w, "too many SSE subscribers", http.StatusServiceUnavailable)
			return
		}
		defer sub.Close()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // for nginx if proxied
		w.WriteHeader(http.StatusOK)

		// Initial comment line so clients know the stream is live even
		// before the first real event arrives.
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		// Replay in-flight run history if a run is active.
		var replayCutoff time.Time
		if replay != nil {
			var evs []engine.Event
			evs, replayCutoff = replay(r.Context())
			for _, ev := range evs {
				if err := writeSSEEvent(w, ev); err != nil {
					// Skip one bad replay entry rather than killing the
					// whole connection before the live stream begins.
					log.Error("sse: failed to marshal replay event",
						"event_type", ev.Type,
						"run_id", ev.RunID,
						"error", err)
				}
			}
		}
		flusher.Flush()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case <-shutdown:
				return
			case ev, ok := <-sub.Chan():
				if !ok {
					return
				}
				// Drop run_log events whose timestamp is within the
				// replay window — those rows were already flushed via
				// ListLogs above, and the bus delivered duplicates that
				// landed between our subscribe + the DB read. Once a
				// fresher run_log arrives, clear the cutoff so we don't
				// keep checking on every future event. (#176)
				if !replayCutoff.IsZero() && ev.Type == engine.EventRunLog {
					if !ev.At.After(replayCutoff) {
						continue
					}
					replayCutoff = time.Time{}
				}
				if err := writeSSEEvent(w, ev); err != nil {
					// A marshal failure means a code defect (e.g. an
					// unmarshallable field was added to an event type).
					// Log it so the bug is visible, then close the stream
					// so the client's EventSource auto-reconnects. (#72)
					log.Error("sse: failed to marshal event",
						"event_type", ev.Type,
						"run_id", ev.RunID,
						"error", err)
					return
				}
				flusher.Flush()
			}
		}
	})
}

func writeSSEEvent(w http.ResponseWriter, ev engine.Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, body)
	return nil
}
