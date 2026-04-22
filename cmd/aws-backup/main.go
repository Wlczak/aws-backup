package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Wlczak/aws-backup/internal/config"
	"github.com/Wlczak/aws-backup/internal/db"
	"github.com/Wlczak/aws-backup/internal/engine"
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
		fmt.Fprintf(os.Stderr, "  run             execute one backup run against the configured source + storage\n\n")
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
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		flag.Usage()
		os.Exit(2)
	}
}

func runBackup(cfgPath string) {
	ctx := context.Background()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fatalf("load %s: %v", cfgPath, err)
	}
	if err := cfg.Validate(); err != nil {
		fatalf("invalid config:\n%v", err)
	}

	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatalf("mkdir %s: %v", dir, err)
	}
	dbPath := filepath.Join(dir, "index.db")
	d, err := db.Open(ctx, dbPath)
	if err != nil {
		fatalf("open db %s: %v", dbPath, err)
	}
	defer d.Close()

	if cfg.Source.Type != config.SourceLocalDir {
		fatalf("only source.type=localdir is supported so far (got %q)", cfg.Source.Type)
	}
	src, err := source.NewLocalDir(cfg.Source.LocalDir.Root)
	if err != nil {
		fatalf("open source: %v", err)
	}
	defer src.Close()

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
		fatalf("init storage: %v", err)
	}
	defer store.Close()

	eng := engine.New(engine.Options{
		DB:        d,
		Source:    src,
		Storage:   store,
		TmpDir:    cfg.Backup.TmpDir,
		KeyPrefix: cfg.S3.KeyPrefix,
		ChunkSize: cfg.Backup.ChunkSize,
		ZipThresh: cfg.Backup.ZipThreshold,
		Emit: func(ev engine.Event) {
			fmt.Printf("[event] %s %+v\n", ev.Type, ev.Data)
		},
	})

	runID, err := eng.Run(ctx)
	if err != nil {
		fatalf("run %d failed: %v", runID, err)
	}
	fmt.Printf("run %d completed. db: %s\n", runID, dbPath)
}

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
