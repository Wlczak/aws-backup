# HTTP API + SSE

Routes mounted in `internal/api/server.go`. JSON over HTTP; the SPA at `/` is served from the same port.

## Endpoints

```text
# Run lifecycle
GET    /api/status                    current + last run, stop_requested flag
GET    /api/runs                      paginated list
GET    /api/runs/{id}                 detail + logs
POST   /api/runs                      trigger run; body {mode: full|scan|upload, paths: []}
POST   /api/runs/{id}/cancel          force-cancel (mid-upload)
POST   /api/runs/{id}/stop            graceful stop between files (#124)
POST   /api/runs/{id}/continue        clear pending stop request

# File index
GET    /api/files                     ?status=&search=&page=&limit=&all=  (limit ≤1000; all=true ≤50k rows, else 400)
GET    /api/files/tree                ?prefix=&status=  immediate children of a folder (lazy tree view)
GET    /api/files/subtree-ids         ?prefix=&status=  every file id+path under prefix (cap 50k; returns `truncated:true` past the cap)
GET    /api/files/stats               counts by status + restore_status, total size (2s cache)
POST   /api/files/{id}/retry          mark single file pending
POST   /api/files/retry               batch: ids[], or all_failed:true, or paths[]
DELETE /api/files/{id}                delete row (S3 untouched)
DELETE /api/files                     batch delete by ids[]

# Settings
GET    /api/settings                  redacted config + pending_apply flag
PUT    /api/settings                  validate + apply (or queue if a run is in flight)

# Connectivity tests
GET    /api/smb/test                  dial source per current config
GET    /api/s3/test                   HeadBucket round-trip

# Restore (Glacier)
POST   /api/restore/estimate          {paths: [], tier: bulk|standard} → cost + wait estimate (request fee counts actual S3 objects, zip groups count once)
POST   /api/restore/trigger           {paths: [], tier: bulk|standard, days: 1..30} → s3:RestoreObject per unique key; matched rows flip to in_progress (does NOT download)
POST   /api/restore/download/estimate {paths: []} → restored-file estimate for the Download tab (breaks out restored / in_progress / not_restoring; request fee counts actual downloadable S3 objects, zip groups count once)
POST   /api/restore/download          {paths: [], target_dir: "/abs/path"} → downloads only restored S3 objects / zip members to disk and verifies each file against files.md5
POST   /api/restore/sync-status       drains SQS queue, applies restore events to DB
POST   /api/restore/scan/full          HEADs every uploaded/zipped S3 object key and reconciles restore status authoritatively
POST   /api/restore/scan/pending       HEADs only rows currently marked `in_progress`

# Sync / reconcile
POST   /api/sync                      authoritative cloud compare; lists bucket objects + zip indexes, compares them to the locally scanned rows, and resets only rows that are still local but whose objects are no longer present
POST   /api/sync/full                 same authoritative cloud compare (compatibility alias)
POST   /api/sync/delete-cloud-paths   {paths: []} → delete corresponding S3 objects/zips

# Live + SPA
GET    /api/events                    SSE stream
GET    /*                             embedded Svelte SPA (hash router fallback to index.html)
```

`PUT /api/settings` no longer 409s during a run — it persists to disk and stashes the merged config; the post-run goroutine applies it once the run finishes (`pending_apply: true` in the response). See `internal/api/handlers_settings.go`.

## SSE Event Catalogue

Defined in `internal/engine/events.go`; subscribers attach via `internal/events/bus.go`; HTTP bridge in `internal/api/sse.go`.

| Type | Payload (Data fields) |
| --- | --- |
| `run_start` | files_scanned, files_uploaded, bytes_uploaded |
| `run_log` | level, message — replayed as a burst on SSE reconnect (#130) |
| `run_complete` | status, files_scanned, files_uploaded, bytes_uploaded, error_message |
| `scan_progress` | seen, new, changed (#137) |
| `scan_complete` | seen, new, changed, unchanged, missing |
| `upload_plan` | total_files, total_groups, total_bytes (#126) |
| `copy_progress` | file_id, name, bytes_copied, size, percent |
| `upload_start` | group_id |
| `upload_progress` | key, bytes_uploaded, size, percent (#123) |
| `upload_complete` | key, bytes |
| `upload_failed` | error |
| `db_sync_start` | reason: complete \| stop \| cancel (#128) |
| `db_sync_progress` | bytes_synced, total_bytes |
| `db_sync_complete` | — |
| `db_sync_failed` | error |
| `restore_scan_start` | mode, total |
| `restore_scan_progress` | mode, scanned, updated, errors, total |
| `restore_scan_complete` | mode, scanned, updated, errors |
| `restore_scan_failed` | mode, error |
| `restore_request_start` | total |
| `restore_request_progress` | processed, total, keys_requested, keys_already_thawed, errors |
| `restore_request_complete` | total, keys_requested, keys_already_in_progress, keys_already_available, files_affected, bytes_affected, errors |
| `restore_request_failed` | error, processed, total |
| `restore_download_start` | total |
| `restore_download_progress` | processed, total, path, files_written, bytes_written, errors, error |
| `restore_download_complete` | files_written, bytes_written, errors |
| `restore_download_failed` | files_written, bytes_written, errors, error |
