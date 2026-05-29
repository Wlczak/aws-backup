# HTTP API + SSE

Routes mounted in `internal/api/server.go`. JSON over HTTP; the SPA at `/` is served from the same port.

## Endpoints

```text
# Auth
GET    /api/auth/status             password_set + authenticated flag for the login/bootstrap screen
POST   /api/auth/login              {password: "..."} → sets the auth cookie on success
POST   /api/auth/logout             clears the auth cookie

# Run lifecycle
GET    /api/status                    current + last run (includes scan_paused / scan_complete), download mirror snapshot, restore-download current + last, stop_requested flag
GET    /api/runs                      paginated list
GET    /api/runs/{id}                 detail + logs
POST   /api/runs                      trigger run; body {mode: full|scan|upload, paths: []}
POST   /api/runs/{id}/cancel          force-cancel (mid-upload)
POST   /api/runs/{id}/stop            graceful stop between files (#124)
POST   /api/runs/{id}/continue        clear pending stop request
DELETE /api/run-logs                 truncate the run_logs table; runs stay intact
POST   /api/download/full             full mirror download using backup.download_dir; the live job summary includes object count + estimated GET/egress cost for the missing set
POST   /api/download/rescan           refresh the cached mirror snapshot for backup.download_dir without downloading files
POST   /api/download/cancel           cancel the active mirror download or rescan job if one is running

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
GET    /api/profiles                  profiles + active_profile + switch-blocking state
POST   /api/profiles                  {name, clone_active} create a profile; cloned profiles clear s3.bucket + sqs.queue_url
PUT    /api/profiles/active           {name} switch active profile; 409 unless backup/download/restore jobs and pending settings are idle
PUT    /api/profiles/{name}/rename    {name} rename a profile; active-profile rename uses the same idle guard as switching
DELETE /api/profiles/{name}           delete an inactive profile; refuses active/last profile

# Connectivity tests
GET    /api/smb/test                  dial source per current config
GET    /api/s3/test                   HeadBucket round-trip

# Restore (Glacier)
POST   /api/restore/estimate          {paths: [], tier: bulk|standard, days: 1..180} → cost + wait estimate (request fee counts actual S3 objects, zip groups count once; the reserved `index.db` snapshot is excluded)
POST   /api/restore/trigger           {paths: [], tier: bulk|standard, days: 1..180} → starts an async restore job that issues s3:RestoreObject per unique key; returns a restore_job_id immediately and matched rows flip to in_progress (does NOT download)
POST   /api/restore/download/estimate {paths: []} → restored-file estimate for the Download tab (breaks out restored / in_progress / not_restoring; request fee counts actual downloadable S3 objects, zip groups count once; the reserved `index.db` snapshot is excluded)
POST   /api/restore/download          {paths: [], target_dir: "/abs/path", verify_checksum?: bool} → starts a background local download/verify job; live progress is exposed through `/api/status` and SSE `restore_download_*`
POST   /api/restore/sync-status       drains SQS queue, applies restore events to DB
POST   /api/restore/scan/full          HEADs every uploaded/zipped/cloud_only S3 object key and reconciles restore status authoritatively
POST   /api/restore/scan/pending       HEADs only rows currently marked `in_progress`
GET    /api/restore/jobs/{id}         restore-trigger / inventory-sync job lookup for reload recovery

# Sync / reconcile
POST   /api/sync                      authoritative cloud compare; lists bucket objects + zip indexes, compares them to the locally scanned rows, recreates cloud-only objects as `cloud_only`, and normalizes S3-present rows into `uploaded` or `cloud_only`
POST   /api/sync/full                 same authoritative cloud compare (compatibility alias)
POST   /api/sync/delete-cloud-paths   {paths: []} → delete corresponding S3 objects/zips

# Live + SPA
GET    /api/events                    SSE stream
GET    /*                             embedded Svelte SPA (hash router fallback to index.html)
```

The dashboard does not treat `/api/events` as the source of truth for run
state. `GET /api/status` is authoritative for the current/last run, scan
flags, and persisted counters; SSE is a best-effort live-detail channel for
things like restore/download jobs and the active run log. Avoid adding new
dashboard dependencies on high-frequency `scan_progress` / `upload_progress`
frames unless they provide a visible improvement that `/api/status` cannot.

Every `/api/*` route except the three auth endpoints requires a valid signed auth cookie. The SPA assets remain public so the browser can load the login/bootstrap screen, but the data API stays locked until `passwd` has set a password and the user has logged in.

`PUT /api/settings` no longer 409s during a run — it persists to disk and stashes the merged config; the post-run goroutine applies it once the run finishes (`pending_apply: true` in the response). See `internal/api/handlers_settings.go`.

Profiles are single-active at runtime. Each profile has its own config and `index.db` under the OS config directory, optionally maps to one S3 bucket, and shares central server settings. Switching profiles rebuilds source/storage/DB/scheduler/SQS/inventory state and is rejected with 409 while any backup run, mirror download, restore download, or pending settings apply is active. A profile with empty `s3.bucket` can be active with no storage handle; S3-dependent endpoints return 503 until the bucket is configured. Renaming changes only the local profile folder/name; it does not rename buckets, prefixes, or bucket-side objects.

`POST /api/download/full` rejects while a backup run is in flight, reuses the cached download-mirror snapshot for `backup.download_dir` when one exists, and only performs a filesystem scan when the directory has not been scanned yet. The scan updates `download_present` / `download_checked_at` and persists a `download_mirror_snapshots` row keyed by the directory path; reruns can then skip the mirror walk and jump straight to the download phase. Zip-backed rows reuse a cached archive from `backup.tmp_dir` when available; otherwise the job downloads the zip once and extracts only the missing members. `POST /api/download/rescan` runs the scan-only phase and refreshes the cached snapshot without downloading any files. `POST /api/download/cancel` asks the active mirror job to stop and surfaces a cancelled terminal state when the worker exits. The live `/api/status` payload exposes the missing-set object count plus a pessimistic estimated request / egress / total cost so operators can see the maximum likely price before and during the download phase, and also includes the cached snapshot timestamp/counts for the mirror card.

`POST /api/restore/download` now starts a background restore-download job instead of holding the request open. The handler seeds the job summary with the total downloadable bytes before the worker starts, so `/api/status` can replay that byte budget immediately on refresh; the live payload then exposes `restore_download_current` / `restore_download_last`, including the active file path plus `current_bytes` / `current_total_bytes` / `current_percent` while a file is streaming, and the SSE stream emits `restore_download_*` events so the Download tab can show progress, survive reloads, and render the final counts after completion or failure. Restore downloads stage each object in the configured temp cache first, then promote the completed file into the target directory so the destination tree only sees fully written files.

`POST /api/restore/estimate` filters DB by `status IN (uploaded, zipped)` and returns a request/retrieval/standard-storage/egress cost breakdown plus expected wait window. Files whose `restore_status` is already `in_progress` or `restored` are excluded from the estimate; the request-fee count is based on distinct S3 objects, so multiple rows inside one zip still count as one restore request. The bucket's reserved `index.db` snapshot object is also excluded so the UI does not count the local DB as a restorable file or restore status row. The skipped rows are surfaced separately as `already_in_progress_*` / `already_restored_*`.

`POST /api/restore/download/estimate` uses the same DB path matching as `RestoreToDir` to split the selected rows into restored / in_progress / not_restoring buckets, estimate the number of S3 objects and indexed bytes that are actually downloadable, and then price GET requests plus outbound egress. The reserved `index.db` snapshot object is excluded from these counts.

`POST /api/restore/trigger` issues `s3:RestoreObject` for every unique key covering the selected paths and asks AWS to keep the thawed copy in standard storage for `days` (1..180). The request can choose `bulk` or `standard`; Glacier objects aren't readable until they thaw (about 48 h Bulk or 12 h Standard for Deep Archive). Matched DB rows immediately flip to `restore_status='in_progress'` so the UI reflects the request; final state lands via SQS (`s3:ObjectRestore:Completed`) or a HEAD scan. Rows already at `restore_status IN (in_progress, restored)` are filtered out before the S3 call (counted in `files_skipped_*` of the response). Storage-level `RestoreAlreadyInProgress` / `InvalidObjectState` from S3 are still mapped to soft-success counts, not errors.

`POST /api/restore/trigger` now returns `202 Accepted` with a `restore_job_id` and runs in the background under a server-scoped context. The live job state is exposed through `/api/status` as `restore_job_current` / `restore_job_last` and can be fetched directly via `GET /api/restore/jobs/{id}` after a refresh.

`POST /api/restore/inventory/sync` follows the same background-job model. It first emits `restore_manifest_*` SSE events while locating and parsing the latest inventory manifest, then reuses the existing `restore_scan_*` events for the HEAD reconciliation pass.

`POST /api/sync` / `/api/sync/full` keep S3-present rows explicit as either `uploaded` or `cloud_only`. Rows that are `cloud_only` but no longer have a matching S3 object are converted to `missing`; rows absent from the DB but present in S3 are recreated as `cloud_only`. Standalone root objects come straight from the bucket listing, not from prior DB history. Zip-backed rows are relinked through the `zips` table, while `files.md5` always stores the per-file checksum.

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
| `restore_manifest_start` | stage, processed, total, manifest_key |
| `restore_manifest_progress` | stage, processed, total, manifest_key, data_key, keys |
| `restore_manifest_complete` | stage, processed, total, manifest_key, keys |
| `restore_manifest_failed` | stage, error, manifest_key, data_key |
| `restore_request_start` | total |
| `restore_request_progress` | processed, total, keys_requested, keys_already_thawed, errors |
| `restore_request_complete` | total, keys_requested, keys_already_in_progress, keys_already_available, files_affected, bytes_affected, errors |
| `restore_request_failed` | error, processed, total |
| `restore_download_start` | total, total_bytes |
| `restore_download_progress` | processed, total, path, files_written, bytes_written, errors, error, current_path, current_bytes, current_total_bytes, current_percent, file_status |
| `restore_download_complete` | files_written, bytes_written, errors |
| `restore_download_failed` | files_written, bytes_written, errors, error |
| `download_mirror_scan_start` | total, download_after |
| `download_mirror_scan_progress` | scanned, present, missing, total, download_after |
| `download_mirror_scan_complete` | scanned, present, missing, total, download_after |
| `download_mirror_scan_failed` | error, download_after |
| `download_mirror_start` | total |
| `download_mirror_progress` | processed, total, path, files_written, bytes_written, errors, error |
| `download_mirror_complete` | files_written, bytes_written, errors |
| `download_mirror_failed` | files_written, bytes_written, errors, error |
| `download_mirror_cancelled` | files_written, bytes_written, errors, error |
