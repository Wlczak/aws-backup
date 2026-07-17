package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/Wlczak/aws-backup/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func TestAuthSetupCreatesPasswordOnceAndKeepsSetupGate(t *testing.T) {
	ts, deps := newTestServer(t)
	centralPath := filepath.Join(t.TempDir(), "central.json")
	if err := config.SaveCentral(centralPath, config.DefaultCentral()); err != nil {
		t.Fatal(err)
	}
	deps.CentralConfigPath = centralPath
	ts.Config.Handler = NewServer(deps).Router()

	resp, err := ts.Client().Post(ts.URL+"/api/auth/setup", "application/json", bytes.NewBufferString(`{"password":"s3cr3t"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup password status=%d", resp.StatusCode)
	}
	var status authStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.PasswordSet || status.Authenticated || !status.SetupRequired {
		t.Fatalf("status=%+v", status)
	}
	if got := resp.Header.Get("Set-Cookie"); got == "" || !bytes.Contains([]byte(got), []byte("Max-Age=0")) {
		t.Fatalf("setup Set-Cookie = %q, want clearing cookie", got)
	}
	central, err := config.LoadCentral(centralPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(central.Auth.PasswordHash), []byte("s3cr3t")); err != nil {
		t.Fatalf("saved password does not verify: %v", err)
	}

	unauthenticated, err := ts.Client().Get(ts.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated.Body.Close()
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("settings status=%d want %d", unauthenticated.StatusCode, http.StatusUnauthorized)
	}

	login, err := ts.Client().Post(ts.URL+"/api/auth/login", "application/json", bytes.NewBufferString(`{"password":"s3cr3t"}`))
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	if login.StatusCode != http.StatusOK || len(login.Cookies()) != 1 {
		t.Fatalf("login status=%d cookies=%v", login.StatusCode, login.Cookies())
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/status", nil)
	req.AddCookie(login.Cookies()[0])
	gated, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	gated.Body.Close()
	if gated.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("operational status=%d want %d", gated.StatusCode, http.StatusPreconditionRequired)
	}

	duplicate, err := ts.Client().Post(ts.URL+"/api/auth/setup", "application/json", bytes.NewBufferString(`{"password":"other"}`))
	if err != nil {
		t.Fatal(err)
	}
	duplicate.Body.Close()
	if duplicate.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate status=%d want %d", duplicate.StatusCode, http.StatusConflict)
	}
}

func TestSetupCompleteUnlocksOperationalRoutes(t *testing.T) {
	ts, deps := newTestServer(t)
	centralPath := filepath.Join(t.TempDir(), "central.json")
	central := config.DefaultCentral()
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cr3t"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	central.Auth.PasswordHash = string(hash)
	if err := config.SaveCentral(centralPath, central); err != nil {
		t.Fatal(err)
	}
	deps.CentralConfigPath = centralPath
	validated := false
	completed := false
	deps.ValidateSetup = func(_ context.Context, got config.Config) error {
		validated = got.S3.Bucket != "" && got.Source.LocalDir.Root != ""
		return nil
	}
	deps.SetupCompleted = func() { completed = true }
	ts.Config.Handler = NewServer(deps).Router()

	login, err := ts.Client().Post(ts.URL+"/api/auth/login", "application/json", bytes.NewBufferString(`{"password":"s3cr3t"}`))
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	if login.StatusCode != http.StatusOK || len(login.Cookies()) != 1 {
		t.Fatalf("login status=%d cookies=%v", login.StatusCode, login.Cookies())
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/setup/complete", nil)
	req.AddCookie(login.Cookies()[0])
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete status=%d", resp.StatusCode)
	}
	if !validated || !completed {
		t.Fatalf("validated=%v completed=%v", validated, completed)
	}
	saved, err := config.LoadCentral(centralPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.SetupRequired() {
		t.Fatal("setup still required after completion")
	}

	statusReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/status", nil)
	statusReq.AddCookie(login.Cookies()[0])
	statusResp, err := ts.Client().Do(statusReq)
	if err != nil {
		t.Fatal(err)
	}
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("operational status=%d want 200", statusResp.StatusCode)
	}
}
