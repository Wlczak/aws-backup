package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Wlczak/aws-backup/internal/api"
	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/events"
	"github.com/Wlczak/aws-backup/internal/restore"
	"github.com/Wlczak/aws-backup/internal/scheduler"
	"github.com/Wlczak/aws-backup/internal/source"
	"github.com/Wlczak/aws-backup/internal/storage"
	"github.com/robfig/cron/v3"
)

var version = "0.0.0-dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("config", "", "path to config.json (default: OS-specific user config dir)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: aws-backup [flags] <command>\n\n")
		fmt.Fprintf(os.Stderr, "commands:\n")
		fmt.Fprintf(os.Stderr, "  config init     write a default config.json (won't overwrite existing)\n")
		fmt.Fprintf(os.Stderr, "  config path     print the resolved config file path\n")
		fmt.Fprintf(os.Stderr, "  config validate check the config is well-formed\n")
		fmt.Fprintf(os.Stderr, "  run             execute one backup run against the configured source + storage\n")
		fmt.Fprintf(os.Stderr, "  serve           run the HTTP API + scheduler (SIGINT to stop)\n\n")
		fmt.Fprintf(os.Stderr, "flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	path := *configPath
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			fatalf("resolve default config path: %v", err)
		}
		path = p
	}

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	switch args[0] {
	case "config":
		runConfig(path, args[1:])
	case "run":
		runBackup(path)
	case "serve":
		runServe(path)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		flag.Usage()
		os.Exit(2)
	}
}

// appState bundles the long-lived objects shared by the serve command.
// src, store, and sched are guarded by mu because /api/settings can
// hot-swap them while a run is in flight.
type appState struct {
	// mu is an RWMutex (not Mutex) so the API server can share it via
	// Deps.ConfigMu — that way cmd-side cfg writes (applySettings) and
	// api-side cfg writes (Server.updateConfig) serialise on a single
	// lock instead of racing across two. RWMutex.Lock semantics are
	// identical to Mutex.Lock for the existing call sites. (#153)
	mu      sync.RWMutex
	cfg     config.Config
	cfgPath string
	db      *db.DB
	src     source.Source
	store   storage.Storage
	sched   *scheduler.Scheduler
	bus     *events.Bus
	dbPath  string
	logger  *slog.Logger
	// stopRequested is wired from api.Server.IsStopRequested in runServe
	// so the engine can poll for graceful-stop requests between files. (#124)
	stopRequested func() bool
	// sqsConsumer holds the long-running S3 Glacier restore-event
	// consumer when sqs.queue_url is configured. Stored so the API's
	// "Sync restore status" button can call DrainAll alongside the
	// background poll. nil when SQS is disabled.
	sqsConsumer *restore.Consumer
}

// loadAppState loads config + storage, refreshes the local index.db from
// S3 if needed, and opens the SQLite DB and source. When withBootUI is
// true and a download is needed, a transient HTTP page is served on the
// configured server address showing progress with a Cancel button; the
// transient server is shut down before this returns so the real API can
// rebind the port. (#143)
func loadAppState(ctx context.Context, cfgPath string, withBootUI bool) (*appState, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", cfgPath, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config:\n%w", err)
	}

	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	store, err := storage.NewS3Storage(ctx, storage.S3Config{
		Endpoint:           cfg.S3.Endpoint,
		UsePathStyle:       cfg.S3.UsePathStyle,
		Region:             cfg.S3.Region,
		Bucket:             cfg.S3.Bucket,
		AccessKeyID:        cfg.S3.AccessKeyID,
		SecretAccessKey:    cfg.S3.SecretAccessKey,
		StorageClass:       cfg.S3.StorageClass,
		MultipartThreshold: cfg.S3.MultipartThreshold,
	})
	if err != nil {
		return nil, fmt.Errorf("init storage: %w", err)
	}

	dbPath := filepath.Join(dir, "index.db")
	// Pull the index from S3 when either (a) the local copy is missing
	// (fresh install / new machine), or (b) the S3 copy is newer than
	// what's on disk — typical when another machine ran a backup more
	// recently. Done BEFORE db.Open so SQLite never has the file swapped
	// out under it. Best-effort: any failure keeps whatever local state
	// exists. With the boot UI a transient HTTP page is served on the
	// configured server address while the download runs, with a Cancel
	// button that lets the user proceed with whatever local state they
	// have. (#143)
	if withBootUI {
		_ = bootRefreshDBFromS3(ctx, cfg.Server, store, cfg.S3.KeyPrefix, dbPath)
	} else {
		_ = refreshDBFromS3(ctx, store, cfg.S3.KeyPrefix, dbPath)
	}

	d, err := db.Open(ctx, dbPath)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("open db %s: %w", dbPath, err)
	}

	src, err := source.FromConfig(cfg.Source)
	if err != nil {
		d.Close()
		store.Close()
		return nil, fmt.Errorf("open source: %w", err)
	}

	return &appState{
		cfg:     cfg,
		cfgPath: cfgPath,
		db:      d,
		src:     src,
		store:   store,
		bus:     events.NewBus(128),
		dbPath:  dbPath,
	}, nil
}

// dbRefreshNeeded reports whether the local index.db at dst is stale
// enough to warrant a fresh download from S3, and returns the remote
// size when so. ok=false with err=nil means "no action needed" (local
// is up-to-date, or S3 has no copy yet).
func dbRefreshNeeded(ctx context.Context, store storage.Storage, prefix, dst string) (ok bool, size int64, err error) {
	const mtimeSkew = time.Second
	key := dbS3Key(prefix)
	localStat, localErr := os.Stat(dst)
	localMissing := errors.Is(localErr, os.ErrNotExist)
	if localErr != nil && !localMissing {
		return false, 0, localErr
	}
	head, headErr := store.Head(ctx, key)
	if headErr != nil {
		// No remote copy yet (fresh deployment) — nothing to refresh.
		if errors.Is(headErr, storage.ErrNotFound) {
			return false, 0, nil
		}
		// Network / permissions issue: keep local untouched.
		return false, 0, headErr
	}
	if !localMissing {
		// LastModified may be unset (some S3-compatible backends omit it);
		// in that case there's nothing to compare so we leave local alone.
		if head.LastModified.IsZero() {
			return false, 0, nil
		}
		if !head.LastModified.After(localStat.ModTime().Add(mtimeSkew)) {
			return false, 0, nil
		}
	}
	return true, head.Size, nil
}

// refreshDBFromS3 ensures the local index.db at dst is at least as fresh
// as the copy in S3. If the local file is missing it's downloaded; if S3
// reports a newer LastModified (by more than mtimeSkew, to absorb clock
// drift between the writer and S3) the local file is replaced. Returns
// nil when no action was needed, or when the action succeeded. (#143)
func refreshDBFromS3(ctx context.Context, store storage.Storage, prefix, dst string) error {
	need, size, err := dbRefreshNeeded(ctx, store, prefix, dst)
	if err != nil || !need {
		return err
	}
	return downloadDBFromS3(ctx, store, prefix, dst, size, nil)
}

// downloadDBFromS3 downloads the index.db object from S3 and writes it to dst
// atomically: bytes go to dst+".part", are fsynced, then renamed into place.
// A partial file (process kill mid-stream) is therefore never visible at dst,
// so the next startup won't open a truncated SQLite file as the live index.
//
// onProgress, if non-nil, is called with the running byte total at most
// once per ~250ms (and once on the final EOF read). size is the expected
// total — pass 0 if unknown.
func downloadDBFromS3(ctx context.Context, store storage.Storage, prefix, dst string, size int64, onProgress func(read, total int64)) error {
	key := dbS3Key(prefix)
	body, err := store.Get(ctx, key)
	if err != nil {
		return err
	}
	defer body.Close()

	var reader io.Reader = body
	if onProgress != nil {
		reader = &dlProgressReader{
			r:           body,
			total:       size,
			minInterval: 250 * time.Millisecond,
			onProgress:  onProgress,
		}
	}
	// ctxReader so the boot UI's Cancel button (which cancels ctx) actually
	// stops the read loop mid-stream instead of letting io.Copy run to
	// completion. The S3 SDK's reader observes ctx itself; this wrap covers
	// in-process / fake backends that don't.
	reader = &ctxReader{r: reader, ctx: ctx}

	tmp := dst + ".part"
	// 0o600 mirrors the perms applied when the DB is freshly created
	// locally (#55): the index records every backed-up path and must
	// not be world-readable. (#66)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, reader); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// ctxReader returns ctx.Err() from Read once the context is cancelled, so
// io.Copy unwinds promptly when the boot UI's Cancel button fires.
type ctxReader struct {
	r   io.Reader
	ctx context.Context
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// dlProgressReader wraps an io.Reader, calling onProgress at most once per
// minInterval with the running byte total, plus once on the final read.
type dlProgressReader struct {
	r           io.Reader
	total       int64
	read        int64
	minInterval time.Duration
	lastEmit    time.Time
	onProgress  func(read, total int64)
}

func (p *dlProgressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	now := time.Now()
	final := errors.Is(err, io.EOF)
	if p.onProgress != nil && (final || p.lastEmit.IsZero() || now.Sub(p.lastEmit) >= p.minInterval) {
		p.lastEmit = now
		p.onProgress(p.read, p.total)
	}
	return n, err
}

// liveStorage returns the currently-active storage handle. The API
// server calls this on every request so a settings hot-swap (PUT
// /api/settings) is observed without a service restart. (#131)
func (a *appState) liveStorage() storage.Storage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.store
}

// syncRestoreStatus returns the closure used as api.Deps.SyncRestoreStatus.
// nil when no SQS consumer is configured so the handler returns 503 and the
// UI surfaces "no queue configured".
func (a *appState) syncRestoreStatus() func(ctx context.Context) (int, error) {
	if a.sqsConsumer == nil {
		return nil
	}
	return a.sqsConsumer.DrainAll
}

// syncDBToS3 checkpoints the WAL and uploads the DB file to S3, emitting
// db_sync_* progress events through the bus so the dashboard can render
// a progress bar (the index can grow to hundreds of MB). reason is
// "complete" | "stop" | "cancel" depending on how the originating run
// ended; runID is the run that triggered this sync. (#128)
func (a *appState) syncDBToS3(ctx context.Context, runID int64, reason string) error {
	if err := a.db.Checkpoint(ctx); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	// Snapshot the live storage and key prefix together so a concurrent
	// applySettings hot-swap can't make us upload to a closed handle or
	// pair the new client with the old prefix. (#131)
	a.mu.Lock()
	store := a.store
	keyPrefix := a.cfg.S3.KeyPrefix
	a.mu.Unlock()

	f, err := os.Open(a.dbPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()

	publish := func(ev engine.Event) {
		if a.bus != nil {
			a.bus.Publish(ev)
		}
	}
	now := time.Now().UTC()
	publish(engine.Event{
		Type:  engine.EventDBSyncStart,
		RunID: runID,
		At:    now,
		Data:  map[string]any{"reason": reason, "size": size},
	})

	body := engine.NewProgressReader(f, size, engine.DefaultProgressInterval, func(read, total int64) {
		var percent float64
		if total > 0 {
			percent = float64(read) / float64(total) * 100
		}
		publish(engine.Event{
			Type:  engine.EventDBSyncProgress,
			RunID: runID,
			At:    time.Now().UTC(),
			Data: map[string]any{
				"reason":  reason,
				"bytes":   read,
				"total":   total,
				"percent": percent,
			},
		})
	})

	// STANDARD tier: the DB sidecar is read on every restart to seed the
	// local index, so it must be instantly readable without a Glacier
	// restore — same reasoning as the zip .index.txt sidecars. (#125)
	_, putErr := store.PutStandard(ctx, dbS3Key(keyPrefix), body, size)
	if putErr != nil {
		publish(engine.Event{
			Type:  engine.EventDBSyncFailed,
			RunID: runID,
			At:    time.Now().UTC(),
			Data:  map[string]any{"reason": reason, "error": putErr.Error()},
		})
		return putErr
	}
	publish(engine.Event{
		Type:  engine.EventDBSyncComplete,
		RunID: runID,
		At:    time.Now().UTC(),
		Data:  map[string]any{"reason": reason, "size": size},
	})
	return nil
}

// dbS3Key returns the S3 key used to store the index database.
func dbS3Key(prefix string) string {
	if prefix == "" {
		return "index.db"
	}
	return strings.TrimRight(prefix, "/") + "/index.db"
}

func (a *appState) buildEngine(mode engine.RunMode, scanPaths []string) (*engine.Engine, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return engine.New(engine.Options{
		DB:             a.db,
		Source:         a.src,
		Storage:        a.store,
		TmpDir:         a.cfg.Backup.TmpDir,
		KeyPrefix:      a.cfg.S3.KeyPrefix,
		ChunkSize:      a.cfg.Backup.ChunkSize,
		ZipThresh:      a.cfg.Backup.ZipThreshold,
		MinZipDirFiles: a.cfg.Backup.MinZipDirFiles,
		ZipMaxBytes:    a.cfg.Backup.ZipMaxBytes,
		EnableZipIndex: a.cfg.Backup.EnableZipIndex,
		RetryFailed:    a.cfg.Backup.RetryFailed,
		CopyThreads:    a.cfg.Backup.CopyThreads,
		UploadThreads:  a.cfg.Backup.UploadThreads,
		PipelineQueue:  a.cfg.Backup.PipelineQueue,
		Mode:           mode,
		ScanPaths:      scanPaths,
		Emit:           a.bus.Publish,
		StopRequested:  a.stopRequested,
	}), nil
}

// applySettings hot-swaps source, storage, and scheduler when the
// corresponding config section changes. Called by the API after a
// successful PUT /api/settings. All pre-checks happen before any swap
// so a failed step never leaves partial state.
//
// handlePutSettings holds runMu for the duration of this call and
// refuses to run while a backup is in progress, so closing the
// swapped-out src/store eagerly is safe — no engine.Engine can still
// hold a reference to them.
func (a *appState) applySettings(ctx context.Context, prev, next config.Config) error {
	var (
		newSrc   source.Source
		newStore storage.Storage
	)
	sourceChanged := prev.Source != next.Source
	s3Changed := prev.S3 != next.S3
	scheduleChanged := prev.Backup.Schedule != next.Backup.Schedule

	// Pre-validate the cron expression before any swap so applySettings
	// is all-or-nothing: a bad schedule must not leave src/store hot-
	// swapped to the new config while sched.Update later rolls back to
	// the old expression. (#115)
	if scheduleChanged && next.Backup.Schedule != "" {
		if _, err := cron.ParseStandard(next.Backup.Schedule); err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
	}

	if sourceChanged {
		s, err := source.FromConfig(next.Source)
		if err != nil {
			return fmt.Errorf("source: %w", err)
		}
		newSrc = s
	}
	if s3Changed {
		s, err := storage.NewS3Storage(ctx, storage.S3Config{
			Endpoint:           next.S3.Endpoint,
			UsePathStyle:       next.S3.UsePathStyle,
			Region:             next.S3.Region,
			Bucket:             next.S3.Bucket,
			AccessKeyID:        next.S3.AccessKeyID,
			SecretAccessKey:    next.S3.SecretAccessKey,
			StorageClass:       next.S3.StorageClass,
			MultipartThreshold: next.S3.MultipartThreshold,
		})
		if err != nil {
			if newSrc != nil {
				newSrc.Close()
			}
			return fmt.Errorf("storage: %w", err)
		}
		newStore = s
	}

	a.mu.Lock()
	var oldSrc source.Source
	var oldStore storage.Storage
	if newSrc != nil {
		oldSrc = a.src
		a.src = newSrc
		if a.logger != nil {
			a.logger.Info("source hot-swapped", "type", next.Source.Type)
		}
	}
	if newStore != nil {
		oldStore = a.store
		a.store = newStore
		if a.logger != nil {
			a.logger.Info("storage hot-swapped", "endpoint", next.S3.Endpoint, "bucket", next.S3.Bucket)
		}
	}
	a.cfg = next
	sched := a.sched
	a.mu.Unlock()

	// Close swapped-out instances eagerly; no in-flight run holds them
	// because handlePutSettings refuses to run while currentRun != 0.
	if oldSrc != nil {
		if err := oldSrc.Close(); err != nil && a.logger != nil {
			a.logger.Warn("close old source after settings swap", "err", err)
		}
	}
	if oldStore != nil {
		if err := oldStore.Close(); err != nil && a.logger != nil {
			a.logger.Warn("close old storage after settings swap", "err", err)
		}
	}

	if scheduleChanged && sched != nil {
		if err := sched.Update(next.Backup.Schedule); err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
		if a.logger != nil {
			a.logger.Info("schedule updated", "expr", next.Backup.Schedule)
		}
	}
	return nil
}

func (a *appState) close() {
	a.store.Close()
	a.src.Close()
	a.db.Close()
}

func runBackup(cfgPath string) {
	ctx := context.Background()
	app, err := loadAppState(ctx, cfgPath, false)
	if err != nil {
		fatalf("%v", err)
	}
	defer app.close()

	eng := engine.New(engine.Options{
		DB:             app.db,
		Source:         app.src,
		Storage:        app.store,
		TmpDir:         app.cfg.Backup.TmpDir,
		KeyPrefix:      app.cfg.S3.KeyPrefix,
		ChunkSize:      app.cfg.Backup.ChunkSize,
		ZipThresh:      app.cfg.Backup.ZipThreshold,
		MinZipDirFiles: app.cfg.Backup.MinZipDirFiles,
		ZipMaxBytes:    app.cfg.Backup.ZipMaxBytes,
		EnableZipIndex: app.cfg.Backup.EnableZipIndex,
		RetryFailed:    app.cfg.Backup.RetryFailed,
		CopyThreads:    app.cfg.Backup.CopyThreads,
		UploadThreads:  app.cfg.Backup.UploadThreads,
		PipelineQueue:  app.cfg.Backup.PipelineQueue,
		Emit: func(ev engine.Event) {
			fmt.Printf("[event] %s %+v\n", ev.Type, ev.Data)
		},
	})

	runID, err := eng.Run(ctx)
	if err != nil {
		fatalf("run %d failed: %v", runID, err)
	}
	fmt.Printf("run %d completed. db: %s\n", runID, app.dbPath)
}

func runServe(cfgPath string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// Set as default so the boot-UI download progress logs use the same
	// handler as the rest of the server.
	slog.SetDefault(logger)

	app, err := loadAppState(ctx, cfgPath, true)
	if err != nil {
		fatalf("%v", err)
	}
	app.logger = logger
	defer app.close()

	// Build the SQS restore-event consumer (if configured) before NewServer
	// so the "Sync restore status" button can call DrainAll. Best-effort:
	// failure to construct logs a warning and the rest of the server runs.
	if app.cfg.SQS.QueueURL != "" {
		consumer, err := restore.New(ctx, app.cfg.SQS, app.cfg.S3, app.db, logger)
		if err != nil {
			logger.Warn("sqs restore consumer disabled", "err", err)
		} else if consumer != nil {
			app.sqsConsumer = consumer
		}
	}

	srv := api.NewServer(api.Deps{
		DB:                app.db,
		Bus:               app.bus,
		Config:            &app.cfg,
		ConfigPath:        app.cfgPath,
		BuildEngine:       app.buildEngine,
		Storage:           app.liveStorage,
		StoragePrefix:     app.cfg.S3.KeyPrefix,
		ConfigMu:          &app.mu,
		SyncDBToS3:        app.syncDBToS3,
		SyncRestoreStatus: app.syncRestoreStatus(),
		ApplySettings: func(prev, next config.Config) error {
			return app.applySettings(context.Background(), prev, next)
		},
		Logger: logger,
	})
	// Wire the API server's graceful-stop flag into the engine. Set before
	// Router() is mounted so buildEngine, called per-run, sees a non-nil
	// callback. (#124)
	app.stopRequested = srv.IsStopRequested

	sched, err := scheduler.New(app.cfg.Backup.Schedule, func(ctx context.Context) error {
		// POST /api/runs trigger. Direct call — we own the same DB/engine.
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/api/runs", nil)
		w := newDiscardResponse()
		srv.Router().ServeHTTP(w, req)
		// Surface non-success statuses so the scheduler logs the failure
		// instead of silently retrying every tick. 409 (run already in
		// progress) is expected when a manual run overlaps a scheduled
		// tick — keep that case quiet so it doesn't pollute logs. (#114)
		if w.status >= 400 && w.status != http.StatusConflict {
			return fmt.Errorf("backup trigger returned HTTP %d", w.status)
		}
		return nil
	}, logger)
	if err != nil {
		fatalf("scheduler: %v", err)
	}
	app.mu.Lock()
	app.sched = sched
	app.mu.Unlock()
	sched.Start()

	// Drive the SQS consumer's background long-poll loop now that the API
	// server is wired up; the consumer itself was constructed earlier so
	// the "Sync restore status" button can drain on demand alongside this
	// loop.
	if app.sqsConsumer != nil {
		go func() { _ = app.sqsConsumer.Run(ctx) }()
	}

	addr := net.JoinHostPort(app.cfg.Server.Host, strconv.Itoa(app.cfg.Server.Port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("serving", "addr", addr, "config", app.cfgPath)
		serveErr <- httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			fatalf("http: %v", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Stop the scheduler BEFORE the HTTP server. If we let it run during
	// shutdown a tick can fire after httpSrv.Shutdown returns, call
	// srv.Router().ServeHTTP, and spawn a fresh engine goroutine that
	// outlives s.runWg.Wait — which would then race app.close() tearing
	// down DB/Storage. sched.Stop drains the in-flight tick (if any)
	// before returning. (#108)
	sched.Stop()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown", "error", err)
	}
	// httpSrv.Shutdown only waits for HTTP handlers; the engine goroutine
	// spawned by POST /api/runs is detached. Cancel any in-flight run and
	// wait for it before app.close() tears down DB and storage.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("engine shutdown", "error", err)
	}
}

// discardResponse is a minimal http.ResponseWriter for programmatic calls.
type discardResponse struct {
	header http.Header
	status int
}

func newDiscardResponse() *discardResponse { return &discardResponse{header: make(http.Header)} }

func (d *discardResponse) Header() http.Header       { return d.header }
func (d *discardResponse) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardResponse) WriteHeader(s int)          { d.status = s }

func runConfig(path string, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "config requires a subcommand (init | path | validate)")
		os.Exit(2)
	}
	switch args[0] {
	case "init":
		if _, err := os.Stat(path); err == nil {
			fatalf("refusing to overwrite existing config at %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			fatalf("stat %s: %v", path, err)
		}
		if err := config.Save(path, config.Default()); err != nil {
			fatalf("write config: %v", err)
		}
		fmt.Printf("wrote default config to %s\n", path)
	case "path":
		fmt.Println(path)
	case "validate":
		cfg, err := config.Load(path)
		if err != nil {
			fatalf("load %s: %v", path, err)
		}
		if err := cfg.Validate(); err != nil {
			fatalf("invalid config:\n%v", err)
		}
		fmt.Printf("ok: %s\n", path)
	default:
		fmt.Fprintf(os.Stderr, "unknown config subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "aws-backup: "+format+"\n", args...)
	os.Exit(1)
}
