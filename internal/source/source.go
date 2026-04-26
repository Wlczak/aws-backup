// Package source abstracts the "thing we back up". Implementations walk a
// tree, yield entries, and open entries for reading. A localdir walker
// lets the rest of the system work against a plain folder during
// development; the SMB walker plugs into the same interface later.
package source

import (
	"context"
	"io"
	"time"
)

// Entry is a single file discovered by a Walk.
type Entry struct {
	// RelPath is the path relative to the source root, using forward
	// slashes regardless of OS. Never empty; never starts with "/".
	RelPath string
	Size    int64
	ModTime time.Time
}

// WalkFunc is called for each file entry. Returning an error aborts the
// walk with that error. ctx cancellation is checked between entries.
type WalkFunc func(Entry) error

// isValidRelPath rejects paths containing characters that break SQLite's
// UNIQUE index (NUL truncates on bind in some drivers, silently colliding
// distinct rows) or the SSE event stream's data: framing (CR/LF). The
// walker drops these entries with a warning rather than letting them
// poison the index or corrupt the live event log.
func isValidRelPath(rel string) bool {
	for i := 0; i < len(rel); i++ {
		switch rel[i] {
		case 0, '\r', '\n':
			return false
		}
	}
	return true
}

// Source is the interface the engine uses to scan and read input files.
type Source interface {
	// Walk visits every regular file under the source, calling fn for each.
	Walk(ctx context.Context, fn WalkFunc) error

	// Open returns a ReadCloser for the entry at relPath (same forward-slash
	// convention Walk emits). Caller closes.
	Open(ctx context.Context, relPath string) (io.ReadCloser, error)

	// Close releases any resources (network connections, handles).
	Close() error
}
