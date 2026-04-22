// Package api hosts the HTTP layer: chi router, JSON handlers, and the
// Server-Sent Events (SSE) stream that feeds the dashboard.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Wlczak/aws-backup/internal/events"
)

// sseHandler returns an http.Handler that streams events from bus to the
// client over Server-Sent Events. The handler returns when the client
// disconnects (ctx.Done fires) or the subscription is closed.
func sseHandler(bus *events.Bus) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // for nginx if proxied
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		sub := bus.Subscribe()
		defer sub.Close()

		// Initial comment line so clients know the stream is live even
		// before the first real event arrives.
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-sub.Chan():
				if !ok {
					return
				}
				body, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, body)
				flusher.Flush()
			}
		}
	})
}
