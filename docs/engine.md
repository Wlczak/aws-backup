# Engine + Run Lifecycle

The orchestrator lives in `internal/engine/engine.go`. A "run" is a single `Engine.RunWithID(ctx, runID)` invocation that scans, reconciles, and uploads.

## Lifecycle (`engine.RunWithID`)

```text
1.  Open run row; emit run_start.
2.  Scan phase  (modes: full | scan)
    source.Scan → batched UpsertFileBatch, preserve existing bucket-backed
    state on unchanged rows, reclassify vanished uploaded/zipped rows to
    cloud_only and vanished pending/failed rows to missing, emit scan_progress
    per batch, then scan_complete. Update run.files_scanned. If mode=scan,
    jump to finalize.

3.  S3 list  (modes: full | upload)
    Storage.List under KeyPrefix (single round-trip, reused below).

4.  Reconcile from S3
    For every {zipKey}.index.txt whose .zip exists in S3, mark contained files
    'uploaded' in the DB if they're still pending/zipped/failed. Closes the
    crash window between successful Put and DB commit (#43). Orphan sidecars
    whose backing zip is missing are deleted (#121).

5.  listPending → GroupFiles
    Pull rows in pending|failed (gated by RetryFailed). GroupFiles splits the
    pending set by directory + size, and folds tiny sibling folders up into
    parent-level zip pools when that reduces S3 object count; emit upload_plan
    with totals.

6.  Pipeline preparation
    mkdir TmpDir; sweepOrphanTmps removes ind-N tmp files for IDs no longer
    pending (#127). Seed per-directory zip counter from MAX(DB, S3 listing) so
    counter survives DB wipe and never overwrites an existing zip (#116).

7.  Pipeline (concurrent)
    Copy workers (CopyThreads):  source → tmp via copyAndHash / CreateZip
                                 with ensureTmpSpace pre-flight (#138);
                                 emit copy_progress.
    Staging buffer (PipelineQueue) caps tmp disk usage.
    Upload workers (UploadThreads): tmp → S3 with skipIfMatches dedup (#133),
                                    progressReader, ChecksumSHA256, multipart
                                    over MultipartThreshold; emit upload_*.
    Between groups/files the engine polls IsStopRequested for graceful stop.

8.  Finalize
    Drain writeBuffer (batched MarkUploaded). FinishRun with terminal status
    (completed | failed | cancelled | stopped). Emit run_complete.

9.  Post-run (in api goroutine after currentRun cleared)
    Apply any pendingConfig queued via PUT /api/settings during the run.
    maybeSyncDBToS3 snapshots the local index.db to a temp file and
    uploads that sidecar in STANDARD tier (#125) when the run
    completed or was gracefully stopped.
```

## Mirror Download

`POST /api/download/full` is a separate mirror job, not part of `Engine.RunWithID` or the dashboard UI.

- It snapshots `backup.download_dir` from settings and refuses to start while a backup run is active.
- The job scans the mirror directory first and updates `files.download_present` / `files.download_checked_at` so the UI can show what is already on disk.
- It then downloads only rows still missing from that mirror.
- Standalone files are fetched directly into the mirror directory with MD5 verification.
- Zip-backed rows are grouped by archive key. The job reuses a cached zip from `backup.tmp_dir` when present; otherwise it downloads the archive once, then extracts only the missing members and marks each extracted row present.
- Progress is published with `download_mirror_*` SSE events so the dashboard can show scan and download phases separately.

## Run-state Concurrency (`api.Server`)

- `runMu` guards `currentRun`, `currentRunCancel`, and the `pendingConfig` field
- `currentRun` is the in-flight run ID; 0 when idle. Set in `handleTriggerRun`, cleared in the post-run goroutine before the DB-sync step
- `currentRunStopReq` (atomic.Bool) — graceful-stop flag polled by the engine between files (#124)
- `currentRunCancelReq` (atomic.Bool) — distinguishes user `/cancel` from service-shutdown cancel (#128)
- `runWg` waits on the engine goroutine during `Server.Shutdown` so DB/storage tear-down sees no live writers (#85)
- `downloadWg` waits on the full-download goroutine during `Server.Shutdown`
- `cfgMu` (RWMutex) shared with `appState.mu` via `Deps.ConfigMu` so cmd-side and api-side cfg writes serialise on a single lock (#153)

## Restore Subsystem

`internal/restore.Consumer` long-polls AWS SQS for S3 Event Notifications about Glacier restore lifecycle (`s3:ObjectRestore:Post` and `s3:ObjectRestore:Completed`). Started in `appState` when `sqs.queue_url` is non-empty; kept as `appState.sqsConsumer` so `POST /api/restore/sync-status` can call `DrainAll` alongside the background poll.

- `Post` event → `db.MarkRestoreInProgress(s3_key)`
- `Completed` event → `db.MarkRestored(s3_key, expiresAt)` — sets `restore_expires_at` so the UI can warn before the temporary copy expires
- `POST /api/restore/scan/full` HEADs every uploaded/zipped/cloud_only object key in the index, not just standalone rows, so zip-backed and S3-only files reconcile too.
- `POST /api/sync` / `/api/sync/full` is the authoritative cloud compare: S3-present rows stay `uploaded` or `cloud_only`, rows that are `cloud_only` but no longer exist in S3 become `missing`, and S3-only paths missing from the DB are recreated as `cloud_only`.
- `RestoreToDir` downloads matching rows only when `restore_status='restored'`, writes them into an operator-selected absolute directory, extracts zip members selectively, and verifies each restored file against the row's `md5` unless `SkipChecksum` is set. Zip archives have their own row in `zips`, but restore verification still uses the member row's checksum. Rows that are still thawing or never restored are skipped and surfaced in `RestoreStats.Skipped`. The API handler wires progress into `restore_download_*` SSE events with the current path plus byte-level file progress so the Download page can show both the overall restore job and the active file meter, and the Download UI keeps the checksum toggle that flips `SkipChecksum`.
- `/api/restore/download/estimate` uses the same DB path matching as `RestoreToDir` to split the selected rows into restored / in_progress / not_restoring buckets, estimate the number of S3 objects and indexed bytes that are actually downloadable, and then price GET requests plus outbound egress.
- The mirror-download flow is separate from restore: it uses the configured mirror directory, scans it into new download-mirror columns, and then pulls only missing rows. For zip-backed rows it can reuse a cached archive from `backup.tmp_dir` and extract just the missing members.
- The mirror-download job tracks the missing-set object count plus a pessimistic GET/egress/total cost estimate in the live job summary so operators can surface the maximum likely price alongside scan and progress state.
- `run_logs` is auto-trimmed after every `FinishRun` (and once at process startup): the just-finished run is capped to `backup.log_max_per_run` rows (lowest-severity oldest first), and every run finished more than `backup.log_retention_days` days ago has its log rows deleted (the runs row itself is preserved). See `docs/data-model.md`
- `POST /api/restore/estimate` filters DB by `status IN (uploaded, zipped)` and returns a request/retrieval/standard-storage/egress cost breakdown plus expected wait window. Files whose `restore_status` is already `in_progress` or `restored` are excluded from the estimate; the request-fee count is based on distinct S3 objects, so multiple rows inside one zip still count as one restore request. The skipped rows are surfaced separately as `already_in_progress_*` / `already_restored_*`
- `POST /api/restore/trigger` issues `s3:RestoreObject` for every unique key covering the selected paths and asks AWS to keep the thawed copy in standard storage for `days` (1..180). The request can choose `bulk` or `standard`; Glacier objects aren't readable until they thaw (about 48 h Bulk or 12 h Standard for Deep Archive). Matched DB rows immediately flip to `restore_status='in_progress'` so the UI reflects the request; final state lands via SQS (`s3:ObjectRestore:Completed`) or a HEAD scan. Rows already at `restore_status IN (in_progress, restored)` are filtered out before the S3 call (counted in `files_skipped_*` of the response). Storage-level `RestoreAlreadyInProgress` / `InvalidObjectState` from S3 are still mapped to soft-success counts, not errors
- `POST /api/restore/download` reuses `RestoreToDir` to fetch only restored S3 objects back to a local directory and MD5-verify them against the DB rows. Download progress is published on the bus so the UI can show per-file progress while the handler runs.
