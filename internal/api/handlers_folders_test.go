package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestListFolders(t *testing.T) {
	ts, _ := newTestServer(t)
	root := t.TempDir()
	for _, name := range []string{"zeta", "Alpha", "beta"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("not a folder"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got foldersResponse
	resp := getJSON(t, ts, "/api/folders?path="+urlQueryEscape(root), &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	if got.Path != root || got.Parent != filepath.Dir(root) {
		t.Fatalf("path=%q parent=%q", got.Path, got.Parent)
	}
	want := []string{"Alpha", "beta", "zeta"}
	if len(got.Folders) != len(want) {
		t.Fatalf("folders=%v want %v", got.Folders, want)
	}
	for i, name := range want {
		if got.Folders[i].Name != name || got.Folders[i].Path != filepath.Join(root, name) {
			t.Errorf("folder[%d]=%+v want name=%q", i, got.Folders[i], name)
		}
	}
	if len(got.Roots) == 0 {
		t.Fatal("roots is empty")
	}
}

func TestListFoldersValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	root := t.TempDir()
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		path   string
		status int
	}{
		{name: "relative", path: "relative", status: http.StatusBadRequest},
		{name: "file", path: file, status: http.StatusBadRequest},
		{name: "missing", path: filepath.Join(root, "missing"), status: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := ts.Client().Get(ts.URL + "/api/folders?path=" + urlQueryEscape(tt.path))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.status {
				t.Fatalf("status=%d want %d", resp.StatusCode, tt.status)
			}
		})
	}
}

func TestCreateFolder(t *testing.T) {
	ts, _ := newTestServer(t)
	root := t.TempDir()
	body, _ := json.Marshal(createFolderRequest{Parent: root, Name: "new folder"})
	resp, err := ts.Client().Post(ts.URL+"/api/folders", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d want 201", resp.StatusCode)
	}
	var got folderEntry
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(root, "new folder")
	if got.Name != "new folder" || got.Path != wantPath {
		t.Fatalf("folder=%+v want path=%q", got, wantPath)
	}
	if info, err := os.Stat(wantPath); err != nil || !info.IsDir() {
		t.Fatalf("created directory stat=%v err=%v", info, err)
	}

	resp, err = ts.Client().Post(ts.URL+"/api/folders", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate status=%d want 409", resp.StatusCode)
	}
}

func TestCreateFolderValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	root := t.TempDir()
	tests := []createFolderRequest{
		{Parent: "relative", Name: "child"},
		{Parent: root, Name: ""},
		{Parent: root, Name: ".."},
		{Parent: root, Name: "nested/child"},
		{Parent: filepath.Join(root, "missing"), Name: "child"},
	}
	for _, req := range tests {
		body, _ := json.Marshal(req)
		resp, err := ts.Client().Post(ts.URL+"/api/folders", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Errorf("request=%+v status=%d want 4xx", req, resp.StatusCode)
		}
	}
}

func TestFolderRoutesRequireAuthentication(t *testing.T) {
	ts, _ := newAuthTestServer(t, "s3cr3t")

	resp, err := ts.Client().Get(ts.URL + "/api/folders")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET status=%d want 401", resp.StatusCode)
	}

	body, _ := json.Marshal(createFolderRequest{Parent: t.TempDir(), Name: "child"})
	resp, err = ts.Client().Post(ts.URL+"/api/folders", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST status=%d want 401", resp.StatusCode)
	}
}

func urlQueryEscape(value string) string {
	return url.QueryEscape(value)
}
