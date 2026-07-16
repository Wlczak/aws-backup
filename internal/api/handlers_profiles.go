package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type profilesResponse struct {
	Profiles      []ProfileInfo `json:"profiles"`
	ActiveProfile string        `json:"active_profile"`
	SwitchBlocked bool          `json:"switch_blocked"`
	BlockedReason string        `json:"blocked_reason,omitempty"`
}

type createProfileRequest struct {
	Name        string `json:"name"`
	CloneActive bool   `json:"clone_active"`
}

type switchProfileRequest struct {
	Name string `json:"name"`
}

type renameProfileRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleListProfiles(w http.ResponseWriter, _ *http.Request) {
	if s.deps.ListProfiles == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("profiles not configured"))
		return
	}
	profiles, err := s.deps.ListProfiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	blocked, reason := s.profileSwitchBlocked()
	writeJSON(w, http.StatusOK, profilesResponse{
		Profiles:      profiles,
		ActiveProfile: s.deps.ActiveProfile,
		SwitchBlocked: blocked,
		BlockedReason: reason,
	})
}

func (s *Server) handleCreateProfile(w http.ResponseWriter, r *http.Request) {
	if s.deps.CreateProfile == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("profiles not configured"))
		return
	}
	var req createProfileRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	info, err := s.deps.CreateProfile(r.Context(), req.Name, req.CloneActive)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleSwitchProfile(w http.ResponseWriter, r *http.Request) {
	if s.deps.SwitchProfile == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("profiles not configured"))
		return
	}
	var req switchProfileRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}

	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.runMu.Lock()
	s.downloadMu.Lock()
	s.restoreDownloadMu.Lock()
	defer s.restoreDownloadMu.Unlock()
	defer s.downloadMu.Unlock()
	defer s.runMu.Unlock()
	if blocked, reason := s.profileSwitchBlockedLocked(); blocked {
		writeJSON(w, http.StatusConflict, map[string]string{"error": reason})
		return
	}

	rt, err := s.deps.SwitchProfile(r.Context(), req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	s.deps.DB = rt.DB
	s.deps.ConfigPath = rt.ConfigPath
	s.deps.ActiveProfile = rt.Info.Name
	s.deps.RestoreScanner = rt.RestoreScanner
	s.deps.Inventory = rt.Inventory
	s.deps.SyncRestoreStatus = rt.SyncRestoreStatus
	s.updateConfig(rt.Config)
	s.clearProfileCaches()
	writeJSON(w, http.StatusOK, rt.Info)
}

func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	if s.deps.DeleteProfile == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("profiles not configured"))
		return
	}
	name := chi.URLParam(r, "name")
	if name == s.deps.ActiveProfile {
		writeError(w, http.StatusBadRequest, errors.New("cannot delete the active profile"))
		return
	}
	if err := s.deps.DeleteProfile(r.Context(), name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRenameProfile(w http.ResponseWriter, r *http.Request) {
	if s.deps.RenameProfile == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("profiles not configured"))
		return
	}
	oldName := chi.URLParam(r, "name")
	var req renameProfileRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}

	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.runMu.Lock()
	s.downloadMu.Lock()
	s.restoreDownloadMu.Lock()
	defer s.restoreDownloadMu.Unlock()
	defer s.downloadMu.Unlock()
	defer s.runMu.Unlock()
	if oldName == s.deps.ActiveProfile {
		if blocked, reason := s.profileSwitchBlockedLocked(); blocked {
			writeJSON(w, http.StatusConflict, map[string]string{"error": reason})
			return
		}
	}

	rt, active, err := s.deps.RenameProfile(r.Context(), oldName, req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if active {
		s.deps.DB = rt.DB
		s.deps.ConfigPath = rt.ConfigPath
		s.deps.ActiveProfile = rt.Info.Name
		s.deps.RestoreScanner = rt.RestoreScanner
		s.deps.Inventory = rt.Inventory
		s.deps.SyncRestoreStatus = rt.SyncRestoreStatus
		s.updateConfig(rt.Config)
		s.clearProfileCaches()
	}
	writeJSON(w, http.StatusOK, rt.Info)
}

func (s *Server) profileSwitchBlocked() (bool, string) {
	s.runMu.Lock()
	blocked, reason := s.profileSwitchBlockedRunLocked()
	s.runMu.Unlock()
	if blocked {
		return true, reason
	}

	s.downloadMu.Lock()
	if s.currentDownload != nil {
		s.downloadMu.Unlock()
		return true, "cannot switch profiles while a mirror download job is in progress"
	}
	s.downloadMu.Unlock()

	s.restoreDownloadMu.Lock()
	if s.currentRestoreDownload != nil {
		s.restoreDownloadMu.Unlock()
		return true, "cannot switch profiles while a restore download job is in progress"
	}
	s.restoreDownloadMu.Unlock()

	return false, ""
}

func (s *Server) profileSwitchBlockedLocked() (bool, string) {
	if blocked, reason := s.profileSwitchBlockedRunLocked(); blocked {
		return true, reason
	}
	if s.currentDownload != nil {
		return true, "cannot switch profiles while a mirror download job is in progress"
	}
	if s.currentRestoreDownload != nil {
		return true, "cannot switch profiles while a restore download job is in progress"
	}
	return false, ""
}

func (s *Server) profileSwitchBlockedRunLocked() (bool, string) {
	if s.currentRun != 0 {
		return true, "cannot switch profiles while a backup run is in progress"
	}
	if s.pendingConfig != nil {
		return true, "cannot switch profiles while settings are pending apply"
	}
	return false, ""
}

func (s *Server) clearProfileCaches() {
	s.fileResponses.Clear()
}
