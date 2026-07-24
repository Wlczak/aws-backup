package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wlczak/aws-backup/internal/config"
	appupdate "github.com/Wlczak/aws-backup/internal/update"
)

func TestOperationGateMakesUpdateIdleCheckAtomic(t *testing.T) {
	var gate operationGate
	done, ok := gate.start()
	if !ok {
		t.Fatal("first operation rejected")
	}
	if gate.beginUpdate() {
		t.Fatal("update started while operation active")
	}
	done()
	if !gate.beginUpdate() {
		t.Fatal("idle update rejected")
	}
	if _, ok := gate.start(); ok {
		t.Fatal("operation started during update")
	}
	gate.cancelUpdate()
	done, ok = gate.start()
	if !ok {
		t.Fatal("operation rejected after failed update released gate")
	}
	done()
}

func TestUpdateStatusAndPreferenceHandlers(t *testing.T) {
	prefs := config.UpdateConfig{AutoCheck: true}
	s := NewServer(Deps{
		Updater:            appupdate.New("v0.2.0-dirty", slog.Default()),
		GetUpdateSettings:  func() (config.UpdateConfig, error) { return prefs, nil },
		SaveUpdateSettings: func(next config.UpdateConfig) error { prefs = next; return nil },
	})

	rr := httptest.NewRecorder()
	s.handleGetUpdate(rr, httptest.NewRequest(http.MethodGet, "/api/update", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", rr.Code, rr.Body)
	}
	var got updateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CurrentVersion != "v0.2.0-dirty" || !got.AutoCheck {
		t.Fatalf("response=%+v", got)
	}

	rr = httptest.NewRecorder()
	s.handlePutUpdateSettings(rr, httptest.NewRequest(http.MethodPut, "/api/update/settings", bytes.NewBufferString(`{"auto_check":false}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rr.Code, rr.Body)
	}
	if prefs.AutoCheck {
		t.Fatal("auto_check was not saved")
	}
}

func TestInstallUpdateRejectsActiveOperation(t *testing.T) {
	s := NewServer(Deps{
		Updater:               appupdate.New("v0.1.0", slog.Default()),
		RequestUpdateShutdown: func(string) {},
	})
	done, ok := s.operations.start()
	if !ok {
		t.Fatal("operation start rejected")
	}
	defer done()
	rr := httptest.NewRecorder()
	s.handleInstallUpdate(rr, httptest.NewRequest(http.MethodPost, "/api/update/install", bytes.NewBufferString(`{"action":"restart"}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
}
