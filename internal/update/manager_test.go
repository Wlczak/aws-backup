package update

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	selfupdate "github.com/minio/selfupdate"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestAssetName(t *testing.T) {
	tests := map[string]string{
		"linux/amd64":   "aws-backup-linux-amd64",
		"windows/amd64": "aws-backup-windows-amd64.exe",
		"darwin/arm64":  "aws-backup-darwin-arm64",
		"linux/arm64":   "",
	}
	for platform, want := range tests {
		var goos, goarch string
		for i, r := range platform {
			if r == '/' {
				goos, goarch = platform[:i], platform[i+1:]
				break
			}
		}
		if got := assetName(goos, goarch); got != want {
			t.Errorf("assetName(%s)=%q want %q", platform, got, want)
		}
	}
}

func TestCheckIncludesPrereleaseAndUsesExactVersion(t *testing.T) {
	body := `[{
          "tag_name":"v0.2.0","html_url":"https://github.com/Wlczak/aws-backup/releases/tag/v0.2.0",
          "draft":false,"prerelease":true,"published_at":"2026-07-16T21:34:57Z",
          "assets":[{"name":"aws-backup-linux-amd64","browser_download_url":"https://example.com/binary"},{"name":"SHA256SUMS","browser_download_url":"https://example.com/sums"}]
        }]`

	m := New("v0.2.0-dirty", slog.Default())
	m.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return response(body), nil })}
	m.apiURL = "https://api.example.test/releases"
	m.goos, m.goarch = "linux", "amd64"
	status := m.Check(t.Context())
	if status.State != StateAvailable {
		t.Fatalf("state=%s want available (%s)", status.State, status.Error)
	}
	if status.Latest == nil || status.Latest.TagName != "v0.2.0" {
		t.Fatalf("latest=%+v", status.Latest)
	}

	m.version = "v0.2.0"
	m.status.CurrentVersion = "v0.2.0"
	if got := m.Check(t.Context()).State; got != StateUpToDate {
		t.Fatalf("exact state=%s want up_to_date", got)
	}
}

func TestChecksumForRejectsMissingAsset(t *testing.T) {
	_, err := checksumFor([]byte(strings.Repeat("a", 64)+"  another-file\n"), "aws-backup-linux-amd64")
	if err == nil {
		t.Fatal("expected missing checksum error")
	}
}

func TestInstallVerifiesAndAppliesReleaseAsset(t *testing.T) {
	binary := []byte("new executable")
	sum := sha256.Sum256(binary)
	m := New("v0.1.0", slog.Default())
	m.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/SHA256SUMS" {
			return response(hex.EncodeToString(sum[:]) + "  aws-backup-linux-amd64\n"), nil
		}
		return response(string(binary)), nil
	})}
	m.goos, m.goarch = "linux", "amd64"
	m.status = Status{CurrentVersion: "v0.1.0", State: StateAvailable, InstallSupported: true, Latest: &Release{
		TagName: "v0.2.0", AssetName: "aws-backup-linux-amd64", AssetURL: "https://downloads.example.test/binary", ChecksumsURL: "https://downloads.example.test/SHA256SUMS",
	}}
	called := false
	m.apply = func(r io.Reader, opts selfupdate.Options) error {
		got, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if string(got) != string(binary) {
			t.Fatalf("applied %q want %q", got, binary)
		}
		if hex.EncodeToString(opts.Checksum) != hex.EncodeToString(sum[:]) {
			t.Fatal("wrong apply checksum")
		}
		called = true
		return nil
	}
	if _, err := m.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("apply not called")
	}
}

func TestChecksumForAcceptsLegacyOutPrefix(t *testing.T) {
	sum := sha256.Sum256([]byte("binary"))
	manifest := []byte(hex.EncodeToString(sum[:]) + "  out/aws-backup-linux-amd64\n")
	got, err := checksumFor(manifest, "aws-backup-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != hex.EncodeToString(sum[:]) {
		t.Fatal("wrong checksum")
	}
}
