// Package config loads, saves, and validates aws-backup's JSON config file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	SourceLocalDir = "localdir"
	SourceSMB      = "smb"

	StorageClassDeepArchive = "DEEP_ARCHIVE"
	StorageClassGlacier     = "GLACIER"
	StorageClassGlacierIR   = "GLACIER_IR"
	StorageClassStandard    = "STANDARD"

	RedactedMarker = "***"
)

// Config is the full runtime config tree persisted to config.json.
type Config struct {
	Source SourceConfig `json:"source"`
	S3     S3Config     `json:"s3"`
	SQS    SQSConfig    `json:"sqs"`
	Backup BackupConfig `json:"backup"`
	Server ServerConfig `json:"server"`
}

// SQSConfig configures the restore-event consumer. Empty QueueURL
// disables polling entirely. Credentials are reused from S3Config.
type SQSConfig struct {
	QueueURL          string `json:"queue_url"`
	Region            string `json:"region"`
	WaitTimeSeconds   int    `json:"wait_time_seconds"`
	VisibilityTimeout int    `json:"visibility_timeout"`
	MaxMessages       int    `json:"max_messages"`
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
	Endpoint        string `json:"endpoint"`       // empty = real AWS; set for MinIO
	UsePathStyle    bool   `json:"use_path_style"` // true for MinIO
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	StorageClass    string `json:"storage_class"`
	KeyPrefix       string `json:"key_prefix"`
	// MultipartThreshold is the byte size at or above which uploads go
	// through the SDK's multipart transfer manager instead of a single
	// PutObject. 0 = default (5 GiB, S3's hard ceiling for single Put).
	// Lowering it earns parallel-part throughput and finer-grained retry
	// for medium-sized objects at the cost of slightly more S3 requests.
	MultipartThreshold int64 `json:"multipart_threshold"`
}

type BackupConfig struct {
	ChunkSize      int    `json:"chunk_size"`
	TmpDir         string `json:"tmp_dir"`
	Schedule       string `json:"schedule"`
	ZipThreshold   int    `json:"zip_threshold"`
	MinZipDirFiles int    `json:"min_zip_dir_files"`
	// ZipMaxBytes caps one zip's uncompressed byte total; the engine
	// splits larger subtrees at subdirectory boundaries. 0 = default
	// (2 GiB per zip, applied by the engine).
	ZipMaxBytes int64 `json:"zip_max_bytes"`
	// EnableZipIndex uploads a STANDARD-tier `.zip.index.txt` sidecar
	// next to each zip listing its contents, so files in a Deep
	// Archive zip can be listed without a Glacier restore.
	EnableZipIndex bool `json:"enable_zip_index"`
	// RetryFailed controls whether the engine picks up rows with status
	// 'failed' alongside 'pending' on each run. Manual retry via the API
	// works regardless of this flag.
	RetryFailed bool `json:"retry_failed"`
	// CopyThreads is the number of concurrent staging workers
	// (source → tmp, i.e. CreateZip / copyAndHash). 0 or 1 = sequential.
	CopyThreads int `json:"copy_threads"`
	// UploadThreads is the number of concurrent S3 upload workers
	// consuming staged tmp files. 0 or 1 = sequential.
	UploadThreads int `json:"upload_threads"`
	// PipelineQueue caps how many staged groups may sit in tmp waiting
	// for upload, bounding peak tmp disk usage. 0 = auto (max(upload_threads, 1)).
	PipelineQueue int `json:"pipeline_queue"`
}

type ServerConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// Default returns a Config wired for local development (localdir source,
// MinIO endpoint, Standard storage class, 2am daily schedule). The
// storage class is STANDARD rather than DEEP_ARCHIVE because MinIO does
// not implement Glacier tiers — switching to real AWS S3 (clear endpoint)
// is the prompt to also pick DEEP_ARCHIVE in the Settings UI.
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
			StorageClass:    StorageClassStandard,
			KeyPrefix:       "backups/",
		},
		Backup: BackupConfig{
			ChunkSize:      10,
			TmpDir:         filepath.Join(os.TempDir(), "aws-backup"),
			Schedule:       "",
			ZipThreshold:   50,
			MinZipDirFiles: 20,
			ZipMaxBytes:    0, // engine default (2 GiB)
			EnableZipIndex: true,
			RetryFailed:    true,
			CopyThreads:    1,
			UploadThreads:  1,
			PipelineQueue:  0, // auto
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
	applyBackfills(data, &cfg)
	return cfg, nil
}

// applyBackfills sets sensible defaults for fields that were added in a
// later version and are therefore absent from older config.json files.
// Matters because Go can't distinguish "explicit false" from "unmarshal
// zero value" on a bool, so we probe the raw JSON to check presence.
func applyBackfills(data []byte, cfg *Config) {
	var probe struct {
		Backup struct {
			EnableZipIndex *bool `json:"enable_zip_index"`
			CopyThreads    *int  `json:"copy_threads"`
			UploadThreads  *int  `json:"upload_threads"`
		} `json:"backup"`
	}
	_ = json.Unmarshal(data, &probe)
	if probe.Backup.EnableZipIndex == nil {
		cfg.Backup.EnableZipIndex = true
	}
	if probe.Backup.CopyThreads == nil {
		cfg.Backup.CopyThreads = 1
	}
	if probe.Backup.UploadThreads == nil {
		cfg.Backup.UploadThreads = 1
	}
}

// Save atomically and durably writes cfg to path (creates parent dir as
// needed). The standard write-tmp + rename pattern is extended with an
// fsync of the tmp file before rename and an fsync of the parent
// directory after rename, so a hard reset between rename and
// writeback can't leave a zero-byte file at path. (#101)
func Save(path string, cfg Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
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
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	// fsync the parent dir so the rename itself is durable. Best-effort:
	// not all filesystems / OSes support it, and a missing fsync there
	// is far less harmful than a missing fsync on the tmp file.
	if dirF, err := os.Open(dir); err == nil {
		_ = dirF.Sync()
		_ = dirF.Close()
	}
	return nil
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
	if c.S3.MultipartThreshold < 0 {
		errs = append(errs, fmt.Errorf("s3.multipart_threshold must be >= 0 (got %d)", c.S3.MultipartThreshold))
	}
	if c.S3.MultipartThreshold > 5*1024*1024*1024 {
		errs = append(errs, fmt.Errorf("s3.multipart_threshold %d exceeds S3's 5 GiB single-PutObject ceiling", c.S3.MultipartThreshold))
	}
	// Glacier-tier classes are AWS-only; S3-compatible endpoints (MinIO,
	// etc.) reject them with InvalidStorageClass on first upload. Catch it
	// at config time so the foot-gun shows up in the Settings UI instead
	// of the next backup run.
	if c.S3.Endpoint != "" {
		switch c.S3.StorageClass {
		case StorageClassDeepArchive, StorageClassGlacier, StorageClassGlacierIR:
			errs = append(errs, fmt.Errorf(
				"s3.storage_class %q requires AWS S3 (leave s3.endpoint empty); S3-compatible endpoints typically only support STANDARD",
				c.S3.StorageClass,
			))
		}
	}

	if c.SQS.QueueURL != "" {
		if c.SQS.Region == "" && c.S3.Region == "" {
			errs = append(errs, errors.New("sqs.region is required when sqs.queue_url is set (or set s3.region as a fallback)"))
		}
		if c.SQS.WaitTimeSeconds < 0 || c.SQS.WaitTimeSeconds > 20 {
			errs = append(errs, fmt.Errorf("sqs.wait_time_seconds must be in [0,20] (got %d)", c.SQS.WaitTimeSeconds))
		}
		if c.SQS.MaxMessages < 0 || c.SQS.MaxMessages > 10 {
			errs = append(errs, fmt.Errorf("sqs.max_messages must be in [1,10] (got %d)", c.SQS.MaxMessages))
		}
		if c.SQS.VisibilityTimeout < 0 {
			errs = append(errs, fmt.Errorf("sqs.visibility_timeout must be >= 0 (got %d)", c.SQS.VisibilityTimeout))
		}
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
	if c.Backup.ZipMaxBytes < 0 {
		errs = append(errs, fmt.Errorf("backup.zip_max_bytes must be >= 0 (got %d)", c.Backup.ZipMaxBytes))
	}
	if c.Backup.CopyThreads < 0 {
		errs = append(errs, fmt.Errorf("backup.copy_threads must be >= 0 (got %d)", c.Backup.CopyThreads))
	} else if c.Backup.CopyThreads > 64 {
		errs = append(errs, fmt.Errorf("backup.copy_threads must be <= 64 (got %d)", c.Backup.CopyThreads))
	}
	if c.Backup.UploadThreads < 0 {
		errs = append(errs, fmt.Errorf("backup.upload_threads must be >= 0 (got %d)", c.Backup.UploadThreads))
	} else if c.Backup.UploadThreads > 64 {
		errs = append(errs, fmt.Errorf("backup.upload_threads must be <= 64 (got %d)", c.Backup.UploadThreads))
	}
	if c.Backup.PipelineQueue < 0 {
		errs = append(errs, fmt.Errorf("backup.pipeline_queue must be >= 0 (got %d)", c.Backup.PipelineQueue))
	}
	if c.Backup.TmpDir == "" {
		errs = append(errs, errors.New("backup.tmp_dir is required"))
	}
	if c.Backup.Schedule != "" {
		sched, err := cron.ParseStandard(c.Backup.Schedule)
		if err != nil {
			errs = append(errs, fmt.Errorf("backup.schedule invalid: %w", err))
		} else {
			// cron.ParseStandard accepts impossible date combinations like
			// "0 0 30 2 *" (Feb 30) or "0 0 31 4 *" (Apr 31) — they parse
			// fine but Next() returns the zero time, meaning the cron
			// silently never fires. Reject those so a misconfigured
			// schedule fails fast at startup instead of looking healthy.
			// (#111)
			next := sched.Next(time.Now())
			if next.IsZero() || next.After(time.Now().AddDate(5, 0, 0)) {
				errs = append(errs, fmt.Errorf("backup.schedule %q never fires (impossible day-of-month / month combination)", c.Backup.Schedule))
			}
		}
	}

	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		errs = append(errs, fmt.Errorf("server.port %d out of range", c.Server.Port))
	}
	if c.Server.Host == "" {
		errs = append(errs, errors.New("server.host is required"))
	} else if ip := net.ParseIP(c.Server.Host); c.Server.Host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		errs = append(errs, fmt.Errorf("server.host %q must be a loopback address (127.0.0.1 or ::1); binding to external interfaces is not supported", c.Server.Host))
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
