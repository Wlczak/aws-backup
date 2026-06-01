package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestClientLogsRoutes(t *testing.T) {
	ts, _ := newTestServer(t)

	reqBody := clientLogsRequest{
		Entries: []clientLogEntryRequest{
			{
				Timestamp: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
				Level:     "warn",
				Source:    "console",
				Message:   "first warning",
				Route:     "dashboard",
				Context:   map[string]any{"n": 1},
			},
			{
				Timestamp: time.Date(2026, 6, 1, 12, 1, 0, 0, time.UTC),
				Level:     "error",
				Source:    "request",
				Message:   "second error",
				URL:       "/api/runs",
			},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := ts.Client().Post(ts.URL+"/api/client-logs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/client-logs status=%d want 201", resp.StatusCode)
	}

	var list clientLogsListResponse
	getJSON(t, ts, "/api/client-logs?page=1&limit=10", &list)
	if list.Total != 2 || len(list.Logs) != 2 {
		t.Fatalf("list total=%d len=%d want 2", list.Total, len(list.Logs))
	}
	if list.Logs[0].Message != "second error" {
		t.Fatalf("newest log = %q want second error", list.Logs[0].Message)
	}
	if list.Logs[1].Context["n"] != float64(1) {
		t.Fatalf("context=%v want n=1", list.Logs[1].Context)
	}

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/client-logs", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/client-logs status=%d want 200", resp.StatusCode)
	}

	list = clientLogsListResponse{}
	getJSON(t, ts, "/api/client-logs?page=1&limit=10", &list)
	if list.Total != 0 || len(list.Logs) != 0 {
		t.Fatalf("after delete total=%d len=%d want 0", list.Total, len(list.Logs))
	}
}

func TestClientLogsRejectInvalidBody(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := ts.Client().Post(ts.URL+"/api/client-logs", "application/json", bytes.NewBufferString(`{"entries":[{"level":"nope","source":"console","message":"bad"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}
