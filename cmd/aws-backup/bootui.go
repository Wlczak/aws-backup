package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/storage"
)

// bootHTML is the single page served on the configured server address while
// the boot-time index.db download is in flight. It polls /progress and
// posts to /cancel; when the response reports done=true the page stops
// polling and the user reloads (the main app will be serving by then).
const bootHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>aws-backup — preparing</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  body { font-family: system-ui, -apple-system, sans-serif; max-width: 480px; margin: 4rem auto; padding: 0 1rem; color: #1f2937; }
  h1 { font-size: 1.25rem; margin-bottom: 0.5rem; }
  p { margin: 0.5rem 0; }
  .bar { width: 100%; height: 0.75rem; background: #e5e7eb; border-radius: 4px; overflow: hidden; margin: 1.25rem 0 0.5rem; }
  .bar > div { height: 100%; background: #2563eb; transition: width 200ms ease-out; }
  .bar.indeterminate > div { width: 33%; animation: slide 1.4s infinite ease-in-out; }
  @keyframes slide { 0% { transform: translateX(-100%); } 100% { transform: translateX(300%); } }
  .stats { color: #4b5563; font-size: 0.9rem; margin-bottom: 1.5rem; font-variant-numeric: tabular-nums; }
  button { padding: 0.5rem 1rem; cursor: pointer; border: 1px solid #d1d5db; background: #fff; border-radius: 4px; font-size: 0.9rem; }
  button:disabled { opacity: 0.5; cursor: default; }
  .err { color: #b91c1c; }
</style>
</head>
<body>
<h1>Downloading index.db from S3</h1>
<p>The backup index is being pulled from S3. The app will start as soon as this finishes.</p>
<div class="bar indeterminate" id="bar"><div id="fill" style="width:0%"></div></div>
<div class="stats" id="stats">Connecting…</div>
<button id="cancel">Cancel and use local copy</button>
<script>
const fill = document.getElementById('fill');
const bar = document.getElementById('bar');
const stats = document.getElementById('stats');
const cancelBtn = document.getElementById('cancel');
const fmt = b => (b / 1048576).toFixed(1) + ' MiB';
async function poll() {
  let r;
  try { r = await fetch('/progress', { cache: 'no-store' }).then(x => x.json()); }
  catch { setTimeout(poll, 500); return; }
  if (r.total > 0) {
    bar.classList.remove('indeterminate');
    const pct = r.bytes / r.total * 100;
    fill.style.width = pct.toFixed(1) + '%';
    stats.textContent = fmt(r.bytes) + ' / ' + fmt(r.total) + ' (' + pct.toFixed(1) + '%)';
  } else {
    stats.textContent = fmt(r.bytes) + ' downloaded';
  }
  if (r.done) {
    cancelBtn.disabled = true;
    if (r.error) stats.innerHTML = '<span class="err">Error: ' + r.error + '</span>';
    else if (r.cancelled) stats.textContent = 'Cancelled — starting with local index…';
    else stats.textContent = 'Done — starting…';
    return;
  }
  setTimeout(poll, 250);
}
cancelBtn.addEventListener('click', async () => {
  cancelBtn.disabled = true;
  try { await fetch('/cancel', { method: 'POST' }); } catch {}
});
poll();
</script>
</body>
</html>
`

// bootState holds the live state of the boot-time download for the UI to
// poll. All access goes through methods so the HTTP handlers can read
// safely while the downloader writes from another goroutine.
type bootState struct {
	mu        sync.Mutex
	bytes     int64
	total     int64
	done      bool
	cancelled bool
	errMsg    string
}

func (s *bootState) update(read, total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytes = read
	s.total = total
}

func (s *bootState) finish(err error, cancelled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	s.cancelled = cancelled
	if err != nil {
		s.errMsg = err.Error()
	}
}

func (s *bootState) snapshot() (b, t int64, done, cancelled bool, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes, s.total, s.done, s.cancelled, s.errMsg
}

// bootRefreshDBFromS3 checks whether the local index.db needs refreshing
// and, if so, runs a minimal HTTP page on srvCfg's address that shows the
// download in progress with a Cancel button. Hitting Cancel aborts the
// download and the app continues with whatever local DB state exists. The
// transient server is shut down before this returns so runServe can rebind
// the same port for the real API. Best-effort: any error is surfaced but
// loadAppState's caller treats refresh failures as non-fatal.
func bootRefreshDBFromS3(ctx context.Context, srvCfg config.ServerConfig, store storage.Storage, prefix, dst string) error {
	need, size, err := dbRefreshNeeded(ctx, store, prefix, dst)
	if err != nil || !need {
		return err
	}

	addr := net.JoinHostPort(srvCfg.Host, strconv.Itoa(srvCfg.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		// Port unbindable (already in use, etc.). Fall back to a silent
		// download so we don't block boot on the UI's preferred port.
		slog.Warn("boot UI listen failed; downloading silently", "addr", addr, "err", err)
		return downloadDBFromS3(ctx, store, prefix, dst, size, nil)
	}
	slog.Info("downloading index.db from S3 — open the URL to view progress / cancel",
		"url", "http://"+addr+"/", "size", size, "dst", dst)
	return runBootDownload(ctx, listener, store, prefix, dst, size)
}

// runBootDownload owns the HTTP server lifecycle for a single boot-time
// download against a pre-bound listener. Split out from bootRefreshDBFromS3
// so tests can inject a port-0 listener and observe the UI directly.
func runBootDownload(ctx context.Context, listener net.Listener, store storage.Storage, prefix, dst string, size int64) error {
	state := &bootState{total: size}
	dlCtx, dlCancel := context.WithCancel(ctx)
	defer dlCancel()

	var cancelHit atomic.Bool
	cancelDownload := func() {
		if cancelHit.CompareAndSwap(false, true) {
			dlCancel()
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(bootHTML))
	})
	mux.HandleFunc("/progress", func(w http.ResponseWriter, r *http.Request) {
		b, t, done, cancelled, errMsg := state.snapshot()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"bytes":     b,
			"total":     t,
			"done":      done,
			"cancelled": cancelled,
			"error":     errMsg,
		})
	})
	mux.HandleFunc("/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		cancelDownload()
		w.WriteHeader(http.StatusOK)
	})

	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(listener) }()

	started := time.Now()
	dlErr := downloadDBFromS3(dlCtx, store, prefix, dst, size, state.update)

	// "User pressed cancel" looks like context.Canceled to the downloader,
	// but the *parent* ctx is still alive — that's how we tell button-cancel
	// from terminal-Ctrl-C apart. Soft-success on user-cancel: keep whatever
	// local state we have and let the app continue.
	cancelled := cancelHit.Load() && ctx.Err() == nil
	if cancelled {
		dlErr = nil
		slog.Info("index.db download cancelled by user — using local state")
	} else if dlErr == nil {
		slog.Info("downloaded index.db",
			"bytes", state.bytes, "duration", time.Since(started).Round(time.Millisecond))
	}
	state.finish(dlErr, cancelled)

	// Give the page one polling interval (~250ms) plus a small margin to
	// fetch the final state before tearing the server down. Without this
	// the user sees an indeterminate "loading" forever instead of "done".
	// Bail early on ctx cancel (e.g. SIGINT) so a Ctrl-C during boot
	// doesn't add 750 ms of dead wait. (#269)
	select {
	case <-time.After(750 * time.Millisecond):
	case <-ctx.Done():
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
	if e := <-serveErr; e != nil && !errors.Is(e, http.ErrServerClosed) {
		slog.Warn("boot UI server exited with error", "err", e)
	}
	return dlErr
}
