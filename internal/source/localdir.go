package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
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
//
// Per-entry errors (transient I/O, EACCES on a single subtree) are
// logged and skipped instead of aborting the whole walk so one bad ACL
// doesn't wedge every backup forever.
func (l *LocalDir) Walk(ctx context.Context, fn WalkFunc) error {
	return filepath.WalkDir(l.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A read error on the walk root itself (e.g. NFS dropped, root
			// permissions changed after NewLocalDir) must abort the walk:
			// swallowing it returns nil with zero entries, after which a
			// full Scan would reclassify every previously-uploaded row as
			// cloud_only and every pending/failed row as missing from a
			// transient I/O blip. (#253)
			if path == l.root {
				return fmt.Errorf("walk root %s unreadable: %w", l.root, err)
			}
			slog.Warn("localdir walk: per-entry error, skipping", "path", path, "err", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			slog.Warn("localdir walk: stat failed, skipping", "path", path, "err", ierr)
			return nil
		}
		rel, rerr := filepath.Rel(l.root, path)
		if rerr != nil {
			slog.Warn("localdir walk: rel failed, skipping", "path", path, "err", rerr)
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !isValidRelPath(rel) {
			slog.Warn("localdir walk: rejecting path with NUL/CR/LF", "path_bytes", []byte(rel))
			return nil
		}
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
	// inside root. The boundary check uses TrimSuffix on the separator so
	// a root that already ends in os.PathSeparator (root='/' on Unix,
	// 'C:\' on Windows) doesn't synthesize a double-separator prefix that
	// no resolved path could ever match.
	absClean, err := filepath.Abs(abs)
	if err != nil {
		return nil, err
	}
	rootTrim := strings.TrimSuffix(l.root, string(os.PathSeparator))
	if rootTrim != "" {
		if absClean != rootTrim && !strings.HasPrefix(absClean, rootTrim+string(os.PathSeparator)) {
			return nil, errors.New("path escapes source root")
		}
	}
	// The string check above only protects against ../ escapes; a symlink
	// inside the root pointing outside still passes (filepath.Abs doesn't
	// resolve symlinks). Re-check after EvalSymlinks against an evaluated
	// root so a symlink whose target lives outside source is refused.
	// ENOENT is fine — os.Open will surface it. (#244)
	resolved, err := filepath.EvalSymlinks(absClean)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("resolve symlinks: %w", err)
		}
		return os.Open(absClean)
	}
	rootResolved, err := filepath.EvalSymlinks(l.root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	rootResolvedTrim := strings.TrimSuffix(rootResolved, string(os.PathSeparator))
	if rootResolvedTrim != "" {
		if resolved != rootResolvedTrim && !strings.HasPrefix(resolved, rootResolvedTrim+string(os.PathSeparator)) {
			return nil, errors.New("symlink target escapes source root")
		}
	}
	return os.Open(resolved)
}

// Close is a no-op for the local filesystem.
func (l *LocalDir) Close() error { return nil }
