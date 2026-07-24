// Package update checks the project's GitHub releases and safely replaces the
// running executable with a checksum-verified release asset.
package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	selfupdate "github.com/minio/selfupdate"
)

const (
	RepositoryURL = "https://github.com/Wlczak/aws-backup"
	ReleasesURL   = RepositoryURL + "/releases"
	releasesAPI   = "https://api.github.com/repos/Wlczak/aws-backup/releases?per_page=20"
	checksumAsset = "SHA256SUMS"
	maxMetadata   = 2 << 20
	maxChecksums  = 1 << 20
)

type State string

const (
	StateIdle      State = "idle"
	StateChecking  State = "checking"
	StateUpToDate  State = "up_to_date"
	StateAvailable State = "available"
	StateIgnored   State = "ignored"
	StateError     State = "error"
)

type Release struct {
	TagName      string    `json:"tag_name"`
	URL          string    `json:"url"`
	PublishedAt  time.Time `json:"published_at"`
	AssetName    string    `json:"-"`
	AssetURL     string    `json:"-"`
	ChecksumsURL string    `json:"-"`
}

type Status struct {
	CurrentVersion   string   `json:"current_version"`
	State            State    `json:"state"`
	Latest           *Release `json:"latest,omitempty"`
	InstallSupported bool     `json:"install_supported"`
	Error            string   `json:"error,omitempty"`
}

type Manager struct {
	mu      sync.RWMutex
	checkMu sync.Mutex
	client  *http.Client
	version string
	goos    string
	goarch  string
	status  Status
	logger  *slog.Logger
	apiURL  string
	apply   func(io.Reader, selfupdate.Options) error
}

func New(version string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		client:  &http.Client{Timeout: 30 * time.Second},
		version: version,
		goos:    runtime.GOOS,
		goarch:  runtime.GOARCH,
		logger:  logger,
		apiURL:  releasesAPI,
		apply:   selfupdate.Apply,
	}
	m.status = Status{CurrentVersion: version, State: StateIdle, InstallSupported: assetName(m.goos, m.goarch) != ""}
	return m
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.status
	if s.Latest != nil {
		copy := *s.Latest
		s.Latest = &copy
	}
	return s
}

func (m *Manager) Ignore() Status {
	m.mu.Lock()
	if m.status.State == StateAvailable {
		m.status.State = StateIgnored
	}
	m.mu.Unlock()
	return m.Status()
}

func (m *Manager) Check(ctx context.Context) Status {
	m.checkMu.Lock()
	defer m.checkMu.Unlock()
	m.mu.Lock()
	m.status.State = StateChecking
	m.status.Error = ""
	m.mu.Unlock()

	rel, err := m.fetchLatest(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.status.State = StateError
		m.status.Error = err.Error()
		m.logger.Warn("update check failed", "error", err)
		return m.status
	}
	m.status.Latest = rel
	m.status.Error = ""
	if rel.TagName == m.version {
		m.status.State = StateUpToDate
	} else {
		m.status.State = StateAvailable
	}
	return m.status
}

type githubRelease struct {
	TagName     string `json:"tag_name"`
	HTMLURL     string `json:"html_url"`
	Draft       bool   `json:"draft"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (m *Manager) fetchLatest(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "aws-backup/"+m.version)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query GitHub releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query GitHub releases: HTTP %d", resp.StatusCode)
	}
	var releases []githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadata)).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode GitHub releases: %w", err)
	}
	want := assetName(m.goos, m.goarch)
	for _, candidate := range releases {
		if candidate.Draft {
			continue
		}
		published, _ := time.Parse(time.RFC3339, candidate.PublishedAt)
		rel := &Release{TagName: candidate.TagName, URL: candidate.HTMLURL, PublishedAt: published, AssetName: want}
		for _, asset := range candidate.Assets {
			switch asset.Name {
			case want:
				rel.AssetURL = asset.URL
			case checksumAsset:
				rel.ChecksumsURL = asset.URL
			}
		}
		return rel, nil
	}
	return nil, errors.New("no published GitHub release found")
}

func assetName(goos, goarch string) string {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return "aws-backup-linux-amd64"
	case "windows/amd64":
		return "aws-backup-windows-amd64.exe"
	case "darwin/amd64":
		return "aws-backup-darwin-amd64"
	case "darwin/arm64":
		return "aws-backup-darwin-arm64"
	default:
		return ""
	}
}

func (m *Manager) Install(ctx context.Context) (*Release, error) {
	status := m.Status()
	if status.Latest == nil || (status.State != StateAvailable && status.State != StateIgnored) {
		return nil, errors.New("no checked update is available")
	}
	rel := *status.Latest
	if rel.AssetName == "" {
		return nil, fmt.Errorf("updates are not published for %s/%s", m.goos, m.goarch)
	}
	if rel.AssetURL == "" {
		return nil, fmt.Errorf("release %s is missing asset %s", rel.TagName, rel.AssetName)
	}
	if rel.ChecksumsURL == "" {
		return nil, fmt.Errorf("release %s is missing %s", rel.TagName, checksumAsset)
	}
	checksums, err := m.download(ctx, rel.ChecksumsURL, maxChecksums)
	if err != nil {
		return nil, fmt.Errorf("download checksums: %w", err)
	}
	expected, err := checksumFor(checksums, rel.AssetName)
	if err != nil {
		return nil, err
	}
	binary, err := m.download(ctx, rel.AssetURL, 256<<20)
	if err != nil {
		return nil, fmt.Errorf("download update: %w", err)
	}
	actual := sha256.Sum256(binary)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), hex.EncodeToString(expected)) {
		return nil, errors.New("downloaded update checksum does not match SHA256SUMS")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	if err := m.apply(bytes.NewReader(binary), selfupdate.Options{TargetPath: exe, Checksum: expected}); err != nil {
		if rollback := selfupdate.RollbackError(err); rollback != nil {
			return nil, fmt.Errorf("replace executable: %w (rollback also failed: %v)", err, rollback)
		}
		return nil, fmt.Errorf("replace executable: %w", err)
	}
	return &rel, nil
}

func (m *Manager) download(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return nil, errors.New("release asset URL is not HTTPS")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "aws-backup/"+m.version)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("release asset exceeds size limit")
	}
	return data, nil
}

func checksumFor(manifest []byte, asset string) ([]byte, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(manifest)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || filepath.Base(strings.TrimPrefix(fields[1], "*")) != asset {
			continue
		}
		value, err := hex.DecodeString(fields[0])
		if err != nil || len(value) != sha256.Size {
			return nil, fmt.Errorf("invalid checksum for %s", asset)
		}
		return value, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%s has no checksum for %s", checksumAsset, asset)
}
