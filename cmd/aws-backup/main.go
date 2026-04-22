package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Wlczak/aws-backup/internal/api"
	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/engine"
	"github.com/Wlczak/aws-backup/internal/events"
	"github.com/Wlczak/aws-backup/internal/scheduler"
	"github.com/Wlczak/aws-backup/internal/source"
	"github.com/Wlczak/aws-backup/internal/storage"
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
type appState struct {
	cfg     config.Config
	cfgPath string
	db      *db.DB
	src     source.Source
	store   storage.Storage
	bus     *events.Bus
	dbPath  string
}

func loadAppState(ctx context.Context, cfgPath string) (*appState, error) {
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
	dbPath := filepath.Join(dir, "index.db")
	d, err := db.Open(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", dbPath, err)
	}

	src, err := source.FromConfig(cfg.Source)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("open source: %w", err)
	}

	store, err := storage.NewS3Storage(ctx, storage.S3Config{
		Endpoint:        cfg.S3.Endpoint,
		UsePathStyle:    cfg.S3.UsePathStyle,
		Region:          cfg.S3.Region,
		Bucket:          cfg.S3.Bucket,
		AccessKeyID:     cfg.S3.AccessKeyID,
		SecretAccessKey: cfg.S3.SecretAccessKey,
		StorageClass:    cfg.S3.StorageClass,
	})
	if err != nil {
		d.Close()
		src.Close()
		return nil, fmt.Errorf("init storage: %w", err)
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

func (a *appState) buildEngine() (*engine.Engine, error) {
	return engine.New(engine.Options{
		DB:        a.db,
		Source:    a.src,
		Storage:   a.store,
		TmpDir:    a.cfg.Backup.TmpDir,
		KeyPrefix: a.cfg.S3.KeyPrefix,
		ChunkSize: a.cfg.Backup.ChunkSize,
		ZipThresh: a.cfg.Backup.ZipThreshold,
		Emit:      a.bus.Publish,
	}), nil
}

func (a *appState) close() {
	a.store.Close()
	a.src.Close()
	a.db.Close()
}

func runBackup(cfgPath string) {
	ctx := context.Background()
	app, err := loadAppState(ctx, cfgPath)
	if err != nil {
		fatalf("%v", err)
	}
	defer app.close()

	eng, _ := app.buildEngine()
	eng = engine.New(engine.Options{
		DB:        app.db,
		Source:    app.src,
		Storage:   app.store,
		TmpDir:    app.cfg.Backup.TmpDir,
		KeyPrefix: app.cfg.S3.KeyPrefix,
		ChunkSize: app.cfg.Backup.ChunkSize,
		ZipThresh: app.cfg.Backup.ZipThreshold,
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

	app, err := loadAppState(ctx, cfgPath)
	if err != nil {
		fatalf("%v", err)
	}
	defer app.close()

	srv := api.NewServer(api.Deps{
		DB:          app.db,
		Bus:         app.bus,
		Config:      &app.cfg,
		ConfigPath:  app.cfgPath,
		BuildEngine: app.buildEngine,
		Logger:      logger,
	})

	sched, err := scheduler.New(app.cfg.Backup.Schedule, func(ctx context.Context) error {
		// POST /api/runs trigger. Direct call — we own the same DB/engine.
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/api/runs", nil)
		w := &discardResponse{}
		srv.Router().ServeHTTP(w, req)
		return nil
	}, logger)
	if err != nil {
		fatalf("scheduler: %v", err)
	}
	sched.Start()
	defer sched.Stop()

	addr := fmt.Sprintf("%s:%d", app.cfg.Server.Host, app.cfg.Server.Port)
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
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown", "error", err)
	}
}

// discardResponse is a minimal http.ResponseWriter for programmatic calls.
type discardResponse struct{ status int }

func (d *discardResponse) Header() http.Header      { return http.Header{} }
func (d *discardResponse) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardResponse) WriteHeader(s int)        { d.status = s }

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
