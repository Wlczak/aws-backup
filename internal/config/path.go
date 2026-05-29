package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

const appDirName = "aws-backup"

var profileNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// DefaultPath returns the OS-appropriate config file path:
//   - Linux:   $XDG_CONFIG_HOME/aws-backup/config.json (falls back to ~/.config/...)
//   - macOS:   ~/Library/Application Support/aws-backup/config.json
//   - Windows: %AppData%\aws-backup\config.json
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, appDirName, "config.json"), nil
}

// DefaultDir returns the directory that DefaultPath lives in — handy for
// parking the sqlite DB and tmp working files next to the config.
func DefaultDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, appDirName), nil
}

// ProfilesDir returns the directory containing every profile subdirectory for
// a central config path.
func ProfilesDir(centralPath string) string {
	return filepath.Join(filepath.Dir(centralPath), "profiles")
}

// ProfileDir returns profiles/<name> for a central config path.
func ProfileDir(centralPath, name string) (string, error) {
	if err := ValidateProfileName(name); err != nil {
		return "", err
	}
	return filepath.Join(ProfilesDir(centralPath), name), nil
}

// ProfilePath returns the per-profile config.json path.
func ProfilePath(centralPath, name string) (string, error) {
	dir, err := ProfileDir(centralPath, name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// ProfileIndexPath returns the per-profile SQLite index path.
func ProfileIndexPath(centralPath, name string) (string, error) {
	dir, err := ProfileDir(centralPath, name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "index.db"), nil
}

// ValidateProfileName keeps profile names safe for direct use as one
// subdirectory under the OS config dir.
func ValidateProfileName(name string) error {
	if name == "." || name == ".." || !profileNameRE.MatchString(name) {
		return fmt.Errorf("invalid profile name %q (use 1-64 letters, numbers, dot, underscore, or dash; start with a letter or number)", name)
	}
	return nil
}
