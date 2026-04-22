// Package config loads, saves, and validates aws-backup's JSON config file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/robfig/cron/v3"
)

const (
	SourceLocalDir = "localdir"
	SourceSMB      = "smb"

	StorageClassDeepArchive = "DEEP_ARCHIVE"
	StorageClassStandard    = "STANDARD"

	RedactedMarker = "***"
)

// Config is the full runtime config tree persisted to config.json.
type Config struct {
	Source SourceConfig `json:"source"`
	S3     S3Config     `json:"s3"`
	Backup BackupConfig `json:"backup"`
	Server ServerConfig `json:"server"`
}

type SourceConfig struct {
	Type     string         `json:"type"` // localdir | smb
	LocalDir LocalDirConfig `json:"localdir"`
	SMB      SMBConfig      `json:"smb"`
}

type LocalDirConfig struct {
	Root string `json:"root"`
}

type SMBConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Domain   string `json:"domain"`
	Share    string `json:"share"`
	Path     string `json:"path"`
}

type S3Config struct {
	Endpoint        string `json:"endpoint"`         // empty = real AWS; set for MinIO
	UsePathStyle    bool   `json:"use_path_style"`   // true for MinIO
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	StorageClass    string `json:"storage_class"`
	KeyPrefix       string `json:"key_prefix"`
}

type BackupConfig struct {
	ChunkSize      int    `json:"chunk_size"`
	TmpDir         string `json:"tmp_dir"`
	Schedule       string `json:"schedule"`
	ZipThreshold   int    `json:"zip_threshold"`
	MinZipDirFiles int    `json:"min_zip_dir_files"`
}

type ServerConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Default returns a Config wired for local development (localdir source,
// MinIO endpoint, DeepArchive storage class, 2am daily schedule).
func Default() Config {
	return Config{
		Source: SourceConfig{
			Type:     SourceLocalDir,
			LocalDir: LocalDirConfig{Root: ""},
			SMB:      SMBConfig{Port: 445},
		},
		S3: S3Config{
			Endpoint:        "http://localhost:9000",
			UsePathStyle:    true,
			Bucket:          "aws-backup-dev",
			Region:          "us-east-1",
			AccessKeyID:     "minioadmin",
			SecretAccessKey: "minioadmin",
			StorageClass:    StorageClassDeepArchive,
			KeyPrefix:       "backups/",
		},
		Backup: BackupConfig{
			ChunkSize:      10,
			TmpDir:         filepath.Join(os.TempDir(), "aws-backup"),
			Schedule:       "0 2 * * *",
			ZipThreshold:   50,
			MinZipDirFiles: 20,
		},
		Server: ServerConfig{
			Host: "127.0.0.1",
			Port: 8080,
		},
	}
}

// Load reads and parses a Config from path. Missing file yields os.ErrNotExist.
func Load(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// Save atomically writes cfg to path (creates parent dir as needed).
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Validate returns nil if the config is internally consistent and usable.
func (c Config) Validate() error {
	var errs []error

	switch c.Source.Type {
	case SourceLocalDir:
		if c.Source.LocalDir.Root == "" {
			errs = append(errs, errors.New("source.localdir.root is required when source.type=localdir"))
		}
	case SourceSMB:
		if c.Source.SMB.Host == "" {
			errs = append(errs, errors.New("source.smb.host is required when source.type=smb"))
		}
		if c.Source.SMB.Share == "" {
			errs = append(errs, errors.New("source.smb.share is required when source.type=smb"))
		}
		if c.Source.SMB.Port <= 0 || c.Source.SMB.Port > 65535 {
			errs = append(errs, fmt.Errorf("source.smb.port %d out of range", c.Source.SMB.Port))
		}
	case "":
		errs = append(errs, errors.New("source.type is required (localdir | smb)"))
	default:
		errs = append(errs, fmt.Errorf("source.type %q invalid (want localdir | smb)", c.Source.Type))
	}

	if c.S3.Bucket == "" {
		errs = append(errs, errors.New("s3.bucket is required"))
	}
	if c.S3.Region == "" {
		errs = append(errs, errors.New("s3.region is required"))
	}
	if c.S3.StorageClass == "" {
		errs = append(errs, errors.New("s3.storage_class is required"))
	}

	if c.Backup.ChunkSize <= 0 {
		errs = append(errs, fmt.Errorf("backup.chunk_size must be > 0 (got %d)", c.Backup.ChunkSize))
	}
	if c.Backup.ZipThreshold < 0 {
		errs = append(errs, fmt.Errorf("backup.zip_threshold must be >= 0 (got %d)", c.Backup.ZipThreshold))
	}
	if c.Backup.MinZipDirFiles < 0 {
		errs = append(errs, fmt.Errorf("backup.min_zip_dir_files must be >= 0 (got %d)", c.Backup.MinZipDirFiles))
	}
	if c.Backup.TmpDir == "" {
		errs = append(errs, errors.New("backup.tmp_dir is required"))
	}
	if c.Backup.Schedule != "" {
		if _, err := cron.ParseStandard(c.Backup.Schedule); err != nil {
			errs = append(errs, fmt.Errorf("backup.schedule invalid: %w", err))
		}
	}

	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		errs = append(errs, fmt.Errorf("server.port %d out of range", c.Server.Port))
	}
	if c.Server.Host == "" {
		errs = append(errs, errors.New("server.host is required"))
	}

	return errors.Join(errs...)
}

// Redacted returns a copy with credential-like fields replaced by RedactedMarker.
// Safe to marshal back to a client.
func (c Config) Redacted() Config {
	out := c
	if out.Source.SMB.Password != "" {
		out.Source.SMB.Password = RedactedMarker
	}
	if out.S3.SecretAccessKey != "" {
		out.S3.SecretAccessKey = RedactedMarker
	}
	if out.S3.AccessKeyID != "" {
		out.S3.AccessKeyID = RedactedMarker
	}
	return out
}
