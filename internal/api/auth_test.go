package api

import (
	"bytes"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/Wlczak/aws-backup/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func newAuthTestServer(t *testing.T, password string) (*testServer, Deps) {
	t.Helper()

	ts, deps := newTestServer(t)
	centralPath := filepath.Join(t.TempDir(), "central.json")
	central := config.DefaultCentral()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	central.Auth.PasswordHash = string(hash)
	if err := config.SaveCentral(centralPath, central); err != nil {
		t.Fatalf("save central: %v", err)
	}

	deps.CentralConfigPath = centralPath
	srv := NewServer(deps)
	ts.Config.Handler = srv.Router()
	return ts, deps
}

func TestAuthStatusLoginLogout(t *testing.T) {
	ts, _ := newAuthTestServer(t, "s3cr3t")

	var status authStatusResponse
	getJSON(t, ts, "/api/auth/status", &status)
	if !status.PasswordSet || status.Authenticated {
		t.Fatalf("status = %+v, want password_set=true authenticated=false", status)
	}

	resp, err := ts.Client().Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want 401", resp.StatusCode)
	}

	resp, err = ts.Client().Post(ts.URL+"/api/auth/login", "application/json", bytes.NewBufferString(`{"password":"wrong"}`))
	if err != nil {
		t.Fatalf("POST /api/auth/login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-password status = %d, want 401", resp.StatusCode)
	}

	loginResp, err := ts.Client().Post(ts.URL+"/api/auth/login", "application/json", bytes.NewBufferString(`{"password":"s3cr3t"}`))
	if err != nil {
		t.Fatalf("POST /api/auth/login: %v", err)
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", loginResp.StatusCode)
	}
	cookies := loginResp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %v, want 1 cookie", cookies)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/status", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(cookies[0])
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/status with cookie: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authed status code = %d, want 200", resp.StatusCode)
	}

	logoutReq, err := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/logout", nil)
	if err != nil {
		t.Fatalf("new logout request: %v", err)
	}
	logoutReq.AddCookie(cookies[0])
	logoutResp, err := ts.Client().Do(logoutReq)
	if err != nil {
		t.Fatalf("POST /api/auth/logout: %v", err)
	}
	defer logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", logoutResp.StatusCode)
	}
	if got := logoutResp.Header.Get("Set-Cookie"); got == "" || !bytes.Contains([]byte(got), []byte("Max-Age=0")) {
		t.Fatalf("logout Set-Cookie = %q, want clearing cookie", got)
	}
}

func TestAuthStatusUnlockedWithoutPassword(t *testing.T) {
	ts, deps := newTestServer(t)
	centralPath := filepath.Join(t.TempDir(), "central.json")
	if err := config.SaveCentral(centralPath, config.DefaultCentral()); err != nil {
		t.Fatalf("save central: %v", err)
	}
	deps.CentralConfigPath = centralPath
	srv := NewServer(deps)
	ts.Config.Handler = srv.Router()

	var status authStatusResponse
	getJSON(t, ts, "/api/auth/status", &status)
	if status.PasswordSet || status.Authenticated {
		t.Fatalf("status = %+v, want unlocked=false/false", status)
	}

	resp, err := ts.Client().Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want 401", resp.StatusCode)
	}
}
