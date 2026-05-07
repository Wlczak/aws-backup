# Engine + Run Lifecycle

The orchestrator lives in `internal/engine/engine.go`. A "run" is a single `Engine.RunWithID(ctx, runID)` invocation that scans, reconciles, and uploads.

## Lifecycle (`engine.RunWithID`)

```text
1.  Open run row; emit run_start.
2.  Scan phase  (modes: full | scan)
    source.Scan → batched UpsertFileBatch, mark missing, emit scan_progress per batch,
    then scan_complete. Update run.files_scanned. If mode=scan, jump to finalize.

3.  S3 list  (modes: full | upload)
    Storage.List under KeyPrefix (single round-trip, reused below).

4.  Reconcile from S3
    For every {zipKey}.index.txt whose .zip exists in S3, mark contained files
    'uploaded' in the DB if they're still pending/zipped/failed. Closes the
    crash window between successful Put and DB commit (#43). Orphan sidecars
    whose backing zip is missing are deleted (#121).

5.  listPending → GroupFiles
    Pull rows in pending|failed (gated by RetryFailed). GroupFiles splits the
    pending set by directory + size; emit upload_plan with totals.

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
    maybeSyncDBToS3 uploads the index.db sidecar in STANDARD tier (#125)
    when the run completed or was gracefully stopped.
```

## Run-state Concurrency (`api.Server`)

- `runMu` guards `currentRun`, `currentRunCancel`, and the `pendingConfig` field
- `currentRun` is the in-flight run ID; 0 when idle. Set in `handleTriggerRun`, cleared in the post-run goroutine before the DB-sync step
- `currentRunStopReq` (atomic.Bool) — graceful-stop flag polled by the engine between files (#124)
- `currentRunCancelReq` (atomic.Bool) — distinguishes user `/cancel` from service-shutdown cancel (#128)
- `runWg` waits on the engine goroutine during `Server.Shutdown` so DB/storage tear-down sees no live writers (#85)
- `cfgMu` (RWMutex) shared with `appState.mu` via `Deps.ConfigMu` so cmd-side and api-side cfg writes serialise on a single lock (#153)

## Restore Subsystem

`internal/restore.Consumer` long-polls AWS SQS for S3 Event Notifications about Glacier restore lifecycle (`s3:ObjectRestore:Post` and `s3:ObjectRestore:Completed`). Started in `appState` when `sqs.queue_url` is non-empty; kept as `appState.sqsConsumer` so `POST /api/restore/sync-status` can call `DrainAll` alongside the background poll.

- `Post` event → `db.MarkRestoreInProgress(s3_key)`
- `Completed` event → `db.MarkRestored(s3_key, expiresAt)` — sets `restore_expires_at` so the UI can warn before the temporary copy expires
- `POST /api/restore/estimate` filters DB by `status IN (uploaded, zipped)` and returns a request/retrieval/egress cost breakdown plus expected wait window
- `POST /api/restore/trigger` issues `s3:RestoreObject` for every unique key covering the selected paths and asks AWS to keep the thawed copy in standard storage for `days` (1..30). It does NOT download anything — Glacier objects aren't readable until they thaw (12–48 h Standard tier). Matched DB rows immediately flip to `restore_status='in_progress'` so the UI reflects the request; final state lands via SQS (`s3:ObjectRestore:Completed`) or a HEAD scan. Storage-level `RestoreAlreadyInProgress` / `InvalidObjectState` are mapped to soft-success counts in the response, not errors
