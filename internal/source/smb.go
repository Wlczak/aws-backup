package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path"
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
	cfg  SMBConfig
	conn net.Conn
	sess *smb2.Session
	share *smb2.Share
	mu   sync.Mutex
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

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
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
func (s *SMB) Walk(ctx context.Context, fn WalkFunc) error {
	root := s.rootPath()
	start := root
	if start == "" {
		start = "."
	}
	return fs.WalkDir(s.share.DirFS(""), start, func(p string, d fs.DirEntry, err error) error {
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
		rel := p
		if root != "" {
			rel = strings.TrimPrefix(rel, root)
			rel = strings.TrimPrefix(rel, "/")
		}
		if rel == "" {
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
	f, err = s.share.Open(full)
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
	msg := err.Error()
	for _, needle := range []string{
		"broken pipe",
		"connection reset",
		"connection refused",
		"use of closed network connection",
		"session expired",
		"session not found",
		"reset by peer",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// reconnectLocked tears down the existing connection (best-effort) and
// dials + mounts a fresh one. Caller must hold s.mu.
func (s *SMB) reconnectLocked(ctx context.Context) error {
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

	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	dialer := &net.Dialer{Timeout: s.cfg.DialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
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
	sess, err := d.Dial(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("authenticate: %w", err)
	}
	share, err := sess.Mount(s.cfg.Share)
	if err != nil {
		sess.Logoff()
		conn.Close()
		return fmt.Errorf("mount %s: %w", s.cfg.Share, err)
	}
	s.conn = conn
	s.sess = sess
	s.share = share
	return nil
}

// Close logs off the SMB session and closes the underlying TCP connection.
func (s *SMB) Close() error {
	var first error
	if s.share != nil {
		if err := s.share.Umount(); err != nil && first == nil {
			first = err
		}
	}
	if s.sess != nil {
		if err := s.sess.Logoff(); err != nil && first == nil {
			first = err
		}
	}
	if s.conn != nil {
		if err := s.conn.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
