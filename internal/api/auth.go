package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wlczak/aws-backup/internal/config"
	"golang.org/x/crypto/bcrypt"
)

const (
	authCookieName     = "aws-backup-auth"
	authCookieLifetime = 30 * 24 * time.Hour
	authTokenVersion   = "v1"
)

type authStatusResponse struct {
	PasswordSet   bool `json:"password_set"`
	Authenticated bool `json:"authenticated"`
	SetupRequired bool `json:"setup_required"`
}

type authLoginRequest struct {
	Password string `json:"password"`
}

type authCookiePayload struct {
	Exp   int64  `json:"exp"`
	Nonce string `json:"nonce"`
}

func (s *Server) authConfig() (config.CentralConfig, bool, error) {
	if s.deps.CentralConfigPath == "" {
		return config.CentralConfig{}, false, nil
	}
	central, err := config.LoadCentral(s.deps.CentralConfigPath)
	if err != nil {
		return config.CentralConfig{}, false, err
	}
	if err := central.ValidateCentral(); err != nil {
		return config.CentralConfig{}, false, err
	}
	return central, true, nil
}

func (s *Server) authState(r *http.Request) (authStatusResponse, string, error) {
	central, enabled, err := s.authConfig()
	if err != nil {
		return authStatusResponse{}, "", err
	}
	if !enabled {
		// Tests and non-auth setups can omit the central config path.
		return authStatusResponse{PasswordSet: false, Authenticated: true, SetupRequired: false}, "", nil
	}
	if central.Auth.PasswordHash == "" {
		return authStatusResponse{PasswordSet: false, Authenticated: false, SetupRequired: true}, "", nil
	}
	ok, err := verifyAuthCookie(r, central.Auth.PasswordHash)
	if err != nil {
		return authStatusResponse{}, "", err
	}
	return authStatusResponse{
		PasswordSet:   true,
		Authenticated: ok,
		SetupRequired: central.SetupRequired(),
	}, central.Auth.PasswordHash, nil
}

func (s *Server) authRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deps.CentralConfigPath == "" {
			next.ServeHTTP(w, r)
			return
		}
		state, _, err := s.authState(r)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !state.PasswordSet {
			writeError(w, http.StatusUnauthorized, errors.New("authentication is not configured; run ./aws-backup passwd"))
			return
		}
		if !state.Authenticated {
			writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// setupRequired keeps an authenticated but incomplete installation confined
// to the APIs used by the onboarding wizard.
func (s *Server) setupRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deps.CentralConfigPath == "" {
			next.ServeHTTP(w, r)
			return
		}
		central, _, err := s.authConfig()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !central.SetupRequired() {
			next.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/settings", "/api/folders", "/api/smb/test", "/api/s3/test", "/api/setup/complete":
			next.ServeHTTP(w, r)
		default:
			writeError(w, http.StatusPreconditionRequired, errors.New("initial setup is not complete"))
		}
	})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	state, _, err := s.authState(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	if s.deps.CentralConfigPath == "" {
		writeError(w, http.StatusServiceUnavailable, errors.New("authentication is not configured"))
		return
	}
	var req authLoginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	if strings.TrimSpace(req.Password) == "" {
		writeError(w, http.StatusBadRequest, errors.New("password is required"))
		return
	}

	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	central, err := config.LoadCentral(s.deps.CentralConfigPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if central.Auth.PasswordHash != "" {
		writeError(w, http.StatusConflict, errors.New("password is already configured"))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("hash password: %w", err))
		return
	}
	if central.SetupCompleted == nil {
		central.MarkSetupRequired()
	}
	central.Auth.PasswordHash = string(hash)
	if err := config.SaveCentral(s.deps.CentralConfigPath, central); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("save password: %w", err))
		return
	}
	// Creating the credential is deliberately separate from proving it.
	// Clear any stale cookie and require a normal login before setup can
	// access authenticated settings endpoints.
	http.SetCookie(w, clearAuthCookie())
	writeJSON(w, http.StatusOK, authStatusResponse{
		PasswordSet:   true,
		Authenticated: false,
		SetupRequired: true,
	})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	central, enabled, err := s.authConfig()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !enabled {
		writeError(w, http.StatusServiceUnavailable, errors.New("authentication is not configured"))
		return
	}
	if central.Auth.PasswordHash == "" {
		writeError(w, http.StatusConflict, errors.New("password is not configured; run ./aws-backup passwd"))
		return
	}

	var req authLoginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, errors.New("password is required"))
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(central.Auth.PasswordHash), []byte(req.Password)); err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("invalid password"))
		return
	}
	cookie, err := buildAuthCookie(central.Auth.PasswordHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	http.SetCookie(w, cookie)
	writeJSON(w, http.StatusOK, authStatusResponse{
		PasswordSet:   true,
		Authenticated: true,
		SetupRequired: central.SetupRequired(),
	})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if s.deps.CentralConfigPath == "" {
		http.SetCookie(w, clearAuthCookie())
		writeJSON(w, http.StatusOK, authStatusResponse{PasswordSet: false, Authenticated: true})
		return
	}
	state, _, err := s.authState(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	http.SetCookie(w, clearAuthCookie())
	state.Authenticated = false
	writeJSON(w, http.StatusOK, state)
}

func verifyAuthCookie(r *http.Request, passwordHash string) (bool, error) {
	c, err := r.Cookie(authCookieName)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			return false, nil
		}
		return false, err
	}
	return validateAuthToken(passwordHash, c.Value)
}

func buildAuthCookie(passwordHash string) (*http.Cookie, error) {
	token, exp, err := makeAuthToken(passwordHash)
	if err != nil {
		return nil, err
	}
	return &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  exp,
		MaxAge:   int(authCookieLifetime.Seconds()),
	}, nil
}

func clearAuthCookie() *http.Cookie {
	return &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(0, 0).UTC(),
		MaxAge:   -1,
	}
}

func makeAuthToken(passwordHash string) (string, time.Time, error) {
	exp := time.Now().UTC().Add(authCookieLifetime)
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, err
	}
	payload := authCookiePayload{
		Exp:   exp.Unix(),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", time.Time{}, err
	}
	sig := signAuthPayload(passwordHash, b)
	token := authTokenVersion + "." + base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(sig)
	return token, exp, nil
}

func validateAuthToken(passwordHash, token string) (bool, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != authTokenVersion {
		return false, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false, nil
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false, nil
	}
	if !hmac.Equal(sig, signAuthPayload(passwordHash, payload)) {
		return false, nil
	}
	var p authCookiePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return false, nil
	}
	if p.Exp <= time.Now().UTC().Unix() {
		return false, nil
	}
	return true, nil
}

func signAuthPayload(passwordHash string, payload []byte) []byte {
	mac := hmac.New(sha256.New, []byte(passwordHash))
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}
