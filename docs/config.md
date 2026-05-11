# Configuration

Defined in `internal/config/config.go`. JSON file; `config.Save` writes via tmp + atomic rename + fsync of file and parent dir (#101). Credentials are returned as `"***"` by `GET /api/settings`; PUT preserves any field equal to `RedactedMarker`.

## Schema (config.json)

```jsonc
{
  "source": {
    "type": "localdir",                  // "localdir" | "smb"
    "localdir": { "root": "" },
    "smb": {
      "host": "", "port": 445,
      "username": "", "password": "", "domain": "",
      "share": "", "path": ""
    }
  },
  "s3": {
    "endpoint": "",                      // empty = real AWS; set for MinIO
    "use_path_style": false,             // true for MinIO
    "bucket": "", "region": "",
    "access_key_id": "", "secret_access_key": "",
    "storage_class": "STANDARD",         // DEEP_ARCHIVE for production
    "key_prefix": "backups/",
    "multipart_threshold": 0             // bytes; 0 = SDK default (5 GiB)
  },
  "sqs": {
    "queue_url": "",                     // empty = SQS consumer disabled
    "region": "",
    "wait_time_seconds": 20,
    "visibility_timeout": 60,
    "max_messages": 10
  },
  "backup": {
    "chunk_size": 100,                   // batch size for source.Scan upserts
    "tmp_dir": "",                       // empty = OS temp
    "schedule": "",                      // empty = manual only; otherwise standard cron
    "zip_threshold": 50,                 // files-per-dir threshold for zipping
    "min_zip_dir_files": 0,              // optional floor on per-zip file count
    "zip_max_bytes": 0,                  // 0 = engine default (2 GiB per zip)
    "enable_zip_index": true,            // upload .zip.index.txt sidecar in STANDARD
    "retry_failed": true,                // pick up status='failed' alongside 'pending'
    "copy_threads": 0,                   // 0/1 = sequential staging
    "upload_threads": 0,                 // 0/1 = sequential uploads
    "pipeline_queue": 0,                 // 0 = max(upload_threads, 1)
    "log_retention_days": 30,            // 0 = keep run_logs forever
    "log_max_per_run": 5000              // 0 = no per-run cap
  },
  "server": { "host": "127.0.0.1", "port": 8080 }
}
```

## Hot-reload semantics

`PUT /api/settings` validates, merges redacted secrets, then either applies live or queues:

- **No run in flight**: validate → `applySettings` (hot-swap source/storage/scheduler) → `config.Save` → update in-memory snapshot. Failed save rolls back the swap.
- **Run in flight**: validate → `config.Save` → stash as `pendingConfig`. The post-run goroutine drains pending and applies once `currentRun` clears. Response carries `pending_apply: true`.

Successive PUTs during one run compose against the pending config (not the live one) so a redacted-secret echo doesn't blank a credential the operator just queued.

## Default config path

`config.DefaultPath()` resolves an OS-specific user config dir:

- Linux: `$XDG_CONFIG_HOME/aws-backup/config.json` (or `~/.config/...`)
- macOS: `~/Library/Application Support/aws-backup/config.json`
- Windows: `%AppData%\aws-backup\config.json`

`-config <path>` overrides this. See `internal/config/path.go`.
