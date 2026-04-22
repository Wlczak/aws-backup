package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LocalDir is a Source rooted at a directory on the local filesystem.
// Used during development and tests; the real SMB source conforms to
// the same interface.
type LocalDir struct {
	root string
}

// NewLocalDir validates that root exists and is a directory.
func NewLocalDir(root string) (*LocalDir, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("localdir root %s: %w", abs, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("localdir root %s is not a directory", abs)
	}
	return &LocalDir{root: abs}, nil
}

// Root returns the absolute root path (useful for logging / tests).
func (l *LocalDir) Root() string { return l.root }

// Walk implements Source.Walk using filepath.WalkDir. Symlinks are not
// followed; non-regular files (devices, sockets, dirs) are skipped.
func (l *LocalDir) Walk(ctx context.Context, fn WalkFunc) error {
	return filepath.WalkDir(l.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(l.root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		return fn(Entry{
			RelPath: rel,
			Size:    info.Size(),
			ModTime: info.ModTime().UTC(),
		})
	})
}

// Open resolves relPath under the root, guarding against "../" escape,
// and returns the underlying *os.File.
func (l *LocalDir) Open(_ context.Context, relPath string) (io.ReadCloser, error) {
	clean := filepath.Clean("/" + strings.TrimPrefix(relPath, "/"))
	clean = strings.TrimPrefix(clean, "/")
	abs := filepath.Join(l.root, filepath.FromSlash(clean))

	// Defense in depth: after join+clean the absolute path must still be
	// inside root.
	absClean, err := filepath.Abs(abs)
	if err != nil {
		return nil, err
	}
	if absClean != l.root && !strings.HasPrefix(absClean, l.root+string(os.PathSeparator)) {
		return nil, errors.New("path escapes source root")
	}
	return os.Open(absClean)
}

// Close is a no-op for the local filesystem.
func (l *LocalDir) Close() error { return nil }
