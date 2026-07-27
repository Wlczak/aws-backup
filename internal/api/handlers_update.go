package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Wlczak/aws-backup/internal/config"
)

type updateResponse struct {
	CurrentVersion   string `json:"current_version"`
	State            string `json:"state"`
	Latest           any    `json:"latest,omitempty"`
	InstallSupported bool   `json:"install_supported"`
	Error            string `json:"error,omitempty"`
	AutoCheck        bool   `json:"auto_check"`
}

func (s *Server) updateResponse() (updateResponse, error) {
	if s.deps.Updater == nil {
		return updateResponse{}, errors.New("updater not configured")
	}
	prefs, err := s.deps.GetUpdateSettings()
	if err != nil {
		return updateResponse{}, fmt.Errorf("load update settings: %w", err)
	}
	status := s.deps.Updater.Status()
	return updateResponse{
		CurrentVersion: status.CurrentVersion, State: string(status.State), Latest: status.Latest,
		InstallSupported: status.InstallSupported, Error: status.Error, AutoCheck: prefs.AutoCheck,
	}, nil
}

func (s *Server) handleGetUpdate(w http.ResponseWriter, _ *http.Request) {
	resp, err := s.updateResponse()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Updater == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("updater not configured"))
		return
	}
	s.deps.Updater.Check(r.Context())
	s.handleGetUpdate(w, r)
}

func (s *Server) handleIgnoreUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Updater == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("updater not configured"))
		return
	}
	s.deps.Updater.Ignore()
	s.handleGetUpdate(w, r)
}

func (s *Server) handlePutUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if s.deps.SaveUpdateSettings == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("update settings not configured"))
		return
	}
	var req struct {
		AutoCheck bool `json:"auto_check"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	if err := s.deps.SaveUpdateSettings(config.UpdateConfig{AutoCheck: req.AutoCheck}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.handleGetUpdate(w, r)
}

func (s *Server) handleInstallUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Updater == nil || s.deps.RequestUpdateShutdown == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("updater not configured"))
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	if req.Action != "restart" && req.Action != "shutdown" {
		writeError(w, http.StatusBadRequest, errors.New("action must be restart or shutdown"))
		return
	}
	if !s.operations.beginUpdate() {
		writeError(w, http.StatusConflict, errors.New("backup, download, restore, or update work is in progress"))
		return
	}
	rel, err := s.deps.Updater.Install(r.Context())
	if err != nil {
		s.operations.cancelUpdate()
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "installed", "version": rel.TagName, "action": req.Action})
	go s.deps.RequestUpdateShutdown(req.Action)
}
