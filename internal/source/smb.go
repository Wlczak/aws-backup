package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hirochachacha/go-smb2"
)

// SMBConfig captures everything needed to dial a share.
type SMBConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	Domain   string
	Share    string
	// Path is the subdirectory within the share to treat as the root;
	// empty string = the whole share.
	Path string
	// DialTimeout bounds the TCP dial. Zero => 10s.
	DialTimeout time.Duration
}

// SMB implements Source against a CIFS/SMB2 share via go-smb2.
type SMB struct {
	cfg   SMBConfig
	mu    sync.Mutex
	conn  net.Conn
	sess  *smb2.Session
	share *smb2.Share
}

// NewSMB dials the share and authenticates. The returned *SMB holds the
// live connection — call Close when done.
func NewSMB(cfg SMBConfig) (*SMB, error) {
	if cfg.Host == "" {
		return nil, errors.New("smb: host is required")
	}
	if cfg.Share == "" {
		return nil, errors.New("smb: share is required")
	}
	if cfg.Port == 0 {
		cfg.Port = 445
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := net.DialTimeout("tcp", addr, cfg.DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("smb dial %s: %w", addr, err)
	}

	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     cfg.Username,
			Password: cfg.Password,
			Domain:   cfg.Domain,
		},
	}
	sess, err := d.Dial(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("smb authenticate: %w", err)
	}

	share, err := sess.Mount(cfg.Share)
	if err != nil {
		sess.Logoff()
		conn.Close()
		return nil, fmt.Errorf("smb mount %s: %w", cfg.Share, err)
	}

	return &SMB{cfg: cfg, conn: conn, sess: sess, share: share}, nil
}

// rootPath returns the share-relative path the walker treats as the root.
func (s *SMB) rootPath() string {
	p := strings.TrimPrefix(s.cfg.Path, "/")
	p = strings.TrimSuffix(p, "/")
	return p
}

// Walk visits every regular file under the configured path using
// fs.WalkDir over the share. Entries are emitted with forward-slash
// RelPath just like LocalDir.
//
// Per-entry errors (transient I/O, ACL flap, single bad dir entry) are
// logged and skipped instead of aborting the whole walk: a single
// permission-denied subtree must not wedge every backup forever.
func (s *SMB) Walk(ctx context.Context, fn WalkFunc) error {
	s.mu.Lock()
	share := s.share
	s.mu.Unlock()
	if share == nil {
		return errors.New("smb: share is not connected")
	}

	root := s.rootPath()
	start := root
	if start == "" {
		start = "."
	}
	return fs.WalkDir(share.DirFS(""), start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Warn("smb walk: per-entry error, skipping", "path", p, "err", err)
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
			slog.Warn("smb walk: stat failed, skipping", "path", p, "err", ierr)
			return nil
		}
		rel := p
		if root != "" {
			rel = strings.TrimPrefix(rel, root)
			rel = strings.TrimPrefix(rel, "/")
		}
		if rel == "" {
			return nil
		}
		if !isValidRelPath(rel) {
			slog.Warn("smb walk: rejecting path with NUL/CR/LF", "path_bytes", []byte(rel))
			return nil
		}
		return fn(Entry{
			RelPath: rel,
			Size:    info.Size(),
			ModTime: info.ModTime().UTC(),
		})
	})
}

// Open returns a ReadCloser for relPath (relative to the configured root).
// If the cached SMB session has gone stale (idle timeout, server-side
// drop, transient network blip) the first Open call after the failure
// re-dials and re-mounts the share once before giving up — long backups
// shouldn't die because the share was idle during a slow zip.
func (s *SMB) Open(ctx context.Context, relPath string) (io.ReadCloser, error) {
	root := s.rootPath()
	clean := path.Clean("/" + strings.TrimPrefix(relPath, "/"))
	full := clean
	if root != "" {
		full = path.Join(root, clean)
		if full != root && !strings.HasPrefix(full, root+"/") {
			return nil, errors.New("path escapes SMB root")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.share != nil {
		f, err := s.share.Open(full)
		if err == nil {
			return f, nil
		}
		if !isSMBSessionError(err) {
			return nil, fmt.Errorf("smb open %s: %w", full, err)
		}
		// One reconnect attempt. If it fails, surface the original error so
		// the caller doesn't lose the failure mode.
		if rerr := s.reconnectLocked(ctx); rerr != nil {
			return nil, fmt.Errorf("smb open %s: %w (reconnect failed: %v)", full, err, rerr)
		}
	} else {
		// Previous reconnect tore down state and failed; try once more
		// before giving up so a transient outage doesn't leave us
		// permanently disconnected.
		if rerr := s.reconnectLocked(ctx); rerr != nil {
			return nil, fmt.Errorf("smb open %s: share not connected (reconnect failed: %v)", full, rerr)
		}
	}
	f, err := s.share.Open(full)
	if err != nil {
		return nil, fmt.Errorf("smb open %s after reconnect: %w", full, err)
	}
	return f, nil
}

// isSMBSessionError reports whether err looks like a session-/connection-
// level failure that a reconnect can recover from. Conservative: anything
// network-y (EOF, broken pipe, connection reset, deadline exceeded) plus
// the smb2 "session expired" wrapper. False positives just retry; false
// negatives skip the retry and surface the original error unchanged.
func isSMBSessionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := err.Error()
	for _, needle := range []string{
		"broken pipe",
		"connection reset",
		"connection refused",
		"use of closed network connection",
		"session expired",
		"session not found",
		"reset by peer",
		"i/o timeout",
		"deadline exceeded",
		"network name deleted",
		"tree not found",
		"no route to host",
		"host is unreachable",
		"host is down",
		"network is unreachable",
		"network is down",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// reconnectLocked dials + mounts a fresh connection and atomically swaps
// it into s.{conn,sess,share} only on full success. On failure the
// previous (already-broken) state is torn down and the fields are left
// nil so callers see a clear "not connected" condition rather than
// dereferencing a partially-rebuilt share. Caller must hold s.mu.
func (s *SMB) reconnectLocked(ctx context.Context) error {
	// Tear down the broken connection first; the caller already saw an
	// error from the existing share so it can't be used further. Holding
	// onto it would just leak a fd until the next failure.
	if s.share != nil {
		_ = s.share.Umount()
		s.share = nil
	}
	if s.sess != nil {
		_ = s.sess.Logoff()
		s.sess = nil
	}
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}

	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	dialer := &net.Dialer{Timeout: s.cfg.DialTimeout}
	newConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     s.cfg.Username,
			Password: s.cfg.Password,
			Domain:   s.cfg.Domain,
		},
	}
	newSess, err := d.Dial(newConn)
	if err != nil {
		newConn.Close()
		return fmt.Errorf("authenticate: %w", err)
	}
	newShare, err := newSess.Mount(s.cfg.Share)
	if err != nil {
		newSess.Logoff()
		newConn.Close()
		return fmt.Errorf("mount %s: %w", s.cfg.Share, err)
	}
	// Swap in only after every step succeeded, so a partial failure
	// can't leave s.share nil while s.conn/sess look healthy.
	s.conn = newConn
	s.sess = newSess
	s.share = newShare
	return nil
}

// Close logs off the SMB session and closes the underlying TCP connection.
func (s *SMB) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	if s.share != nil {
		if err := s.share.Umount(); err != nil && first == nil {
			first = err
		}
		s.share = nil
	}
	if s.sess != nil {
		if err := s.sess.Logoff(); err != nil && first == nil {
			first = err
		}
		s.sess = nil
	}
	if s.conn != nil {
		if err := s.conn.Close(); err != nil && first == nil {
			first = err
		}
		s.conn = nil
	}
	return first
}
