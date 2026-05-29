# Configuration

Defined in `internal/config/config.go`. JSON files; `config.Save*` writes via tmp + atomic rename + fsync of file and parent dir (#101). Credentials are returned as `"***"` by `GET /api/settings`; PUT preserves any field equal to `RedactedMarker`.

## Profile layout

The OS config directory now has one central config plus one subdirectory per profile:

```text
aws-backup/
  config.json
  profiles/<profile>/config.json
  profiles/<profile>/index.db
```

Central `config.json` owns shared process settings plus the optional login hash used by the HTTP auth cookie:

```jsonc
{
  "active_profile": "default",
  "server": { "host": "127.0.0.1", "port": 8080 },
  "auth": { "password_hash": "" }
}
```

Each profile config owns `source`, `s3`, `sqs`, and `backup`. A profile maps to at most one bucket and one local SQLite index. Existing single-profile installs are migrated on startup by moving the old runtime config/index into `profiles/default/` and writing the central config at the old `config.json` path. Creating a new profile with `clone_active` copies operational defaults from the active profile but clears `s3.bucket` and `sqs.queue_url`, so the new profile cannot accidentally reuse the active bucket or restore queue. An empty `s3.bucket` means S3 is not configured yet; the profile can still be active, but backup upload, cloud sync, restore, download, inventory, and S3 test actions reject until a bucket is set.
The auth hash stays in the central config, not the per-profile config, so switching profiles does not change the login state. When the hash is empty the HTTP API stays locked until the operator runs `./aws-backup passwd`.

## Effective Settings Schema (`GET /api/settings`)

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
    "scan_batch_bytes": 4294967296,      // soft byte budget for batched full runs; 0/negative rejected, default 4 GiB
    "tmp_dir": "",                       // empty = OS temp
    "download_dir": "",                  // local mirror target used by the full-download mirror job
    "schedule": "",                      // empty = manual only; otherwise standard cron
    "zip_threshold": 50,                 // files-per-dir threshold for zipping
    "min_zip_dir_files": 0,              // optional floor on per-zip file count; folds tiny sibling folders into the current zip pool
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

`PUT /api/settings` validates, merges redacted secrets, saves profile fields to the active profile config, saves server fields to the central config, then either applies live or queues:

- **No run in flight**: validate → `applySettings` (hot-swap source/storage/scheduler) → `config.Save` → update in-memory snapshot. Failed save rolls back the swap.
- **Run in flight**: validate → `config.Save` → stash as `pendingConfig`. The post-run goroutine drains pending and applies once `currentRun` clears. Response carries `pending_apply: true`.

Successive PUTs during one run compose against the pending config (not the live one) so a redacted-secret echo doesn't blank a credential the operator just queued.

`backup.download_dir` is the persistent local mirror target used by the full-download mirror job. It must be an absolute path. The first mirror sync against a new directory performs a bootstrap scan, then caches the result in the database so later reruns can skip the filesystem walk; use `POST /api/download/rescan` or the dashboard button to refresh that cached snapshot when the folder changes on disk.

`./aws-backup passwd` prompts twice in the terminal and writes a bcrypt hash into `auth.password_hash`. The password itself is never stored in plaintext.

## Default config path

`config.DefaultPath()` resolves the central config path in the OS-specific user config dir:

- Linux: `$XDG_CONFIG_HOME/aws-backup/config.json` (or `~/.config/...`)
- macOS: `~/Library/Application Support/aws-backup/config.json`
- Windows: `%AppData%\aws-backup\config.json`

`-config <path>` overrides the central config path. `-profile <name>` overrides `active_profile` for the current process. Profile names are one safe path segment: letters, numbers, dot, underscore, or dash; 1-64 chars; must start with a letter or number. See `internal/config/path.go`.
