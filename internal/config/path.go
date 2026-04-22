package config

import (
	"os"
	"path/filepath"
)

const appDirName = "aws-backup"

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
