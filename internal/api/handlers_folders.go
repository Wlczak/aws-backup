package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type folderEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type foldersResponse struct {
	Path    string        `json:"path"`
	Parent  string        `json:"parent,omitempty"`
	Roots   []folderEntry `json:"roots"`
	Folders []folderEntry `json:"folders"`
}

type createFolderRequest struct {
	Parent string `json:"parent"`
	Name   string `json:"name"`
}

func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = defaultBrowsePath()
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		writeError(w, http.StatusBadRequest, errors.New("path must be absolute"))
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		writeFolderFilesystemError(w, err)
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, errors.New("path must be a directory"))
		return
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		writeFolderFilesystemError(w, err)
		return
	}
	folders := make([]folderEntry, 0, len(entries))
	for _, entry := range entries {
		isDir := entry.IsDir()
		if !isDir && entry.Type()&os.ModeSymlink != 0 {
			target, statErr := os.Stat(filepath.Join(path, entry.Name()))
			isDir = statErr == nil && target.IsDir()
		}
		if isDir {
			folders = append(folders, folderEntry{
				Name: entry.Name(),
				Path: filepath.Join(path, entry.Name()),
			})
		}
	}
	sort.Slice(folders, func(i, j int) bool {
		left, right := strings.ToLower(folders[i].Name), strings.ToLower(folders[j].Name)
		if left == right {
			return folders[i].Name < folders[j].Name
		}
		return left < right
	})

	parent := filepath.Dir(path)
	if parent == path {
		parent = ""
	}
	writeJSON(w, http.StatusOK, foldersResponse{
		Path:    path,
		Parent:  parent,
		Roots:   filesystemRoots(),
		Folders: folders,
	})
}

func (s *Server) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	var req createFolderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid JSON body: "+err.Error()))
		return
	}
	parent := filepath.Clean(req.Parent)
	if req.Parent == "" || !filepath.IsAbs(parent) {
		writeError(w, http.StatusBadRequest, errors.New("parent must be an absolute path"))
		return
	}
	if !validFolderName(req.Name) {
		writeError(w, http.StatusBadRequest, errors.New("name must be a single directory name"))
		return
	}

	info, err := os.Stat(parent)
	if err != nil {
		writeFolderFilesystemError(w, err)
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, errors.New("parent must be a directory"))
		return
	}

	path := filepath.Join(parent, req.Name)
	if err := os.Mkdir(path, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			writeError(w, http.StatusConflict, errors.New("a file or directory with that name already exists"))
			return
		}
		writeFolderFilesystemError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, folderEntry{Name: req.Name, Path: path})
}

func validFolderName(name string) bool {
	return strings.TrimSpace(name) != "" &&
		name != "." && name != ".." &&
		!filepath.IsAbs(name) &&
		filepath.Base(name) == name &&
		!strings.ContainsAny(name, `/\\`) &&
		!strings.ContainsRune(name, 0)
}

func defaultBrowsePath() string {
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		return home
	}
	if cwd, err := os.Getwd(); err == nil && filepath.IsAbs(cwd) {
		return cwd
	}
	return string(filepath.Separator)
}

func writeFolderFilesystemError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, os.ErrPermission):
		writeError(w, http.StatusForbidden, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}
