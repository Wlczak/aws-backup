# Recently Closed Issues

Kept as searchable context for past architectural calls. The git log has the full history.

| # | Commit | Summary |
| --- | --- | --- |
| #38 | db02540 | `MarkMissing` includes `zipped` rows |
| #39 | f1880af | `currentRun` leak + `discardResponse` header on panic |
| #43 | b223d84 | Reconcile DB from S3 zip indexes at backup start |
| #44 | de9b06e | `hasPrefixPath` extracted to shared `pathutil` package |
| #63 | e96f640 | `applySettings` closes swapped-out source/storage; PUT settings was 409 during a run *(superseded by deferred-apply, ad3af15)* |
| #80 | — | Reproducible builds via `-trimpath` |
| #82 | 031f359 | `MarkMissing` now includes `pending` and `failed` rows |
| #85 | e96f640 | `Server.Shutdown` waits on `runWg` before DB/storage teardown |
| #86 | 031f359 | `downloadDBFromS3` writes atomically via `.part` + rename |
| #87 | ac6714a | `HasPrefixPath` tolerates trailing slash on prefix |
| #88 | e96f640 | SMB.Open transparently re-dials + remounts on session-level errors |
| #89 | 031f359 | `handlePutSettings` surfaces rollback errors |
| #90 | 031f359 | `source.Scan` cancels walker via `WithCancelCause` on flush failure |
| #91 | e96f640 | `StoragePrefix` accessed via `cfgMu`-guarded `storagePrefix()` |
| #92 | ac6714a | Upload loop uses partial `UpdateUploadStats`, preserves scan count |
| #93 | ac6714a | `runWithID` returns real `runErr` even when `FinishRun` fails |
| #94 | e96f640 | `deps.Config` accessed via `cfgMu`-guarded `snapshotConfig()` / `updateConfig()` |
| #95 | 8589466 | `processZipGroup` uploads `.index.txt` sidecar before the zip |
| #96 | 031f359 | `LoadCloudIndex` skips `.index.txt` sidecars whose zip is missing |
| #97 | ac6714a | `Storage.List` callers normalize prefix via `pathutil.NormalizeS3ListPrefix` |
| #100 | ac6714a | `handleTriggerRun` goroutine defers `cancel()` to release run context |
| #101 | ebfb6c5 | `config.Save` fsyncs tmp file and parent dir for crash-safe atomic write |
| #102 | ebfb6c5 | LocalDir / SMB walkers log + skip per-entry errors instead of aborting |
| #103 | ebfb6c5 | `ReconcileZip` requires `uploaded_at IS NULL` to avoid rebinding modified files |
| #109 | ebfb6c5 | S3 multipart uploads request composed full-object SHA256 checksum |
| #115 | ebfb6c5 | `applySettings` pre-validates the cron expression before any swap |
| #116 | ebfb6c5 | `Storage.PutIfAbsent` via S3 `IfNoneMatch=*`; engine retries on collision |
| #118 | ebfb6c5 | Drain writeBuffer before `FinishRun` so files_uploaded matches DB |
| #121 | ebfb6c5 | `reconcileFromS3` deletes orphan `.index.txt` sidecars |
| #122 | 29dd2d3 | Default `Backup.Schedule = ""` so fresh installs don't silently cron |
| #123 | f74932e | `upload_progress` SSE event with throttled `progressReader` |
| #124 | 601cce6 | Graceful Stop endpoint + `RunStopped` status; engine polls `StopRequested` |
| #125 | 86a44f7 | `syncDBToS3` uses `PutStandard` so the DB sidecar lands in STANDARD tier |
| #126 | 2613de2 | `upload_plan` SSE event carries totals up front |
| #127 | 79220b4 | Resume cached tmp on retry via stable `ind-{fileID}` name |
| #128 | — | Distinguish user-cancel from shutdown-cancel; post-run DB sync to S3 |
| #130 | — | SSE replay of run_log on reconnect |
| #131 | — | `Deps.Storage` is a getter so handlers see the live storage post-swap |
| #133 | 5aa8908 | Pre-flight `skipIfMatches` dedup before each `Storage.Put` |
| #136 | 9004b24 | Dashboard pins active uploads above active copies |
| #137 | b6fa1e3 | `source.Scan` ProgressFn → `scan_progress` SSE event |
| #138 | d26c7fa | Pre-flight `ensureTmpSpace` with 64 MiB margin (cross-platform) |
| #143 | — | Boot-UI HTTP server during S3 index download |
| #153 | — | `Deps.ConfigMu` shared with cmd-side `appState.mu` |
| #70 | — | Dashboard polls `/api/status` at 30s when idle, 3s during a run |
| #64 | — | `/api/files?all=true` capped at 50k rows; oversized indexes 400 with paginate hint |
| #75 | — | Restore verifies bytes against DB md5 in flight; `RestoreOptions.SkipChecksum` opts out |
| #182 | — | `/api/status` treats a stale `currentRun` id as idle (200 + null current) instead of 500 |
| #181 | — | Scheduler trigger uses `context.Background()`; old 5 min timeout only canceled bookkeeping while the engine ran unbounded |
| #183 | — | `decodeJSON` sanitises CR/LF and caps decoder errors at 256 chars (log-injection guard) |
| #176 | — | SSE replay captures cutoff before `ListLogs`; live forwarder drops `run_log` events with `At <= cutoff` to dedupe overlap |
| #169 | — | `runPipeline` logs which group was abandoned when a retry can't be re-queued during a stop, instead of silently bumping `groupErrCount` |
| #173 | — | Schema management migrated from GORM `AutoMigrate` to goose-embedded SQL migrations; `00002_multipart_unique_indexes.sql` enforces partial UNIQUE on `multipart_uploads(file_id|zip_key)` |
| —    | — | `Stats` MIN(restore_expires_at) scans a `NullString` and parses with multiple layouts; legacy DBs no longer 500 the dashboard on data-format drift |
| — | 4b3b2b9 | Live `copy_progress` SSE event for source→tmp phase |
| — | ea82784 | `S3.MultipartThreshold` config option |
| — | ad3af15 | Settings PUT during a run is queued, not 409'd; applied post-run |
| #170 | — | `reconcileFromS3` skips orphan sidecar delete on cancel — no spurious "orphan delete failed" log on user stop |
| #172 | — | `ListPending` selects only `id, path, size, mtime` (the columns the engine consumes) instead of `SELECT *` |
| #174 | — | `source.Scan` flush goroutine recovers from panic and surfaces it via `flushErrCh` so a bad flush can't deadlock Scan |
| #175 | — | `S3Storage.Get` translates `InvalidObjectState` to new `ErrGlacierThawing` sentinel; restore wraps it as "still thawing" in `RestoreStats.Errors` |
| #190 | — | inventory `parseSchema` strips surrounding double-quotes per column so quoted manifest schemas still match bare names |
| #198 | — | `Dashboard.triggerRun` no longer resets state after the await — `run_start` SSE handler is the sole source of truth |
| #205 | — | `db_sync_failed` clears the pending 8s auto-hide timer so a recent `db_sync_complete` doesn't wipe the failure banner |
| #207 | — | `Files.svelte` clears `searchTimer` and aborts in-flight `load()` writes on unmount |
| #208 | — | `format.bytes()` switched to base-2 (KiB/MiB/GiB) to match StorageSettings + `du` conventions |
| #199 | — | Dashboard upload counters derived from `itemProgress` so SSE replay/reconnect can't double-count |
| #201 | 76093d0 | `subscribeEvents` adds an `onmessage` fallback so untyped SSE frames are forwarded (and warned on in dev) instead of silently dropped |
| #200 | e781690 | Dashboard `itemProgress` evicts oldest terminal items beyond a 200-cap so a 50k-file run doesn't keep every entry in `$state` for the tab lifetime |
| #209 | 45e0789 | Restore double-click confirm resets via `$effect` on `raw`/`targetDir` so editing the path between clicks forces a fresh confirm |
| #211 | 4e64502 | Dashboard/Files/Logs/Restore route polling+action errors through `toast.error` per the project convention; inline `{#if err}` cards removed |
| #202 | 141f9f7 | `Restore.loadInventory` guards on an `aborted` flag so navigating away mid-fetch doesn't write to torn-down `$state` |
| #203 | 552799a | `request<T>` throws a typed `ApiError` with `kind: 'network' \| 'http' \| 'parse'`; HTTP errors carry `status` + first 200 chars of the body |
| #212 | 87770e9 | `selection.pruneToIds(present)`; Files tree-mode load drops dangling selection rows so deletions don't carry into Restore preload |
| #204 | bf6a791 | `api.files` accepts `AbortSignal`; Files.svelte aborts the prior load before each new one; AbortError surfaced as `ApiError` kind=`abort` |
| #197 | 93dd96d | `Server.inventorySyncBusy` CAS-guards `handleInventorySync` so concurrent clicks 409 instead of both downloading the manifest |
| #193 | 93dd96d | `restoreZipMembers` falls back to a case-folded zip-member lookup with a slog.Info "used X" line; ambiguous case-insensitive matches surface as a per-member error |
| #194 | 93dd96d | `writeFromReader` removes the partial dst on any non-checksum error so a zero-byte file doesn't masquerade as a tiny successful restore on retry |
| #188 | dc20388 | `findLatestManifest` skips manifests younger than a 5-minute settle window so a too-fresh manifest doesn't hit NoSuchKey on its data files |
| #189 | dc20388 | `ListLatestKeys` reads LastModified for the chosen manifest and slog.Warns when its age exceeds 2× the configured frequency |
| #192 | dc20388 | Scanner shares a `golang.org/x/time/rate` token bucket (4000 rps, burst 200) across the worker pool to stay under S3's 5500 rps per-prefix HEAD/GET ceiling |
| #191 | dc20388 | `handleRestoreScanFull` and `handleInventorySync` reject with 409 when a backup run is in flight to avoid HEAD-driven status writes racing engine writes on the same `s3_key` |
| #185 | f31f152 | `Consumer.receiveMu` serialises Receive+process between Run and DrainAll so the drain can't fire `OnDrainComplete` while Run is mid-batch |
| #186 | f31f152 | `handleMessage` spawns a heartbeat that calls `ChangeMessageVisibility(visSec)` every visSec/3 so a slow batch can't be redelivered mid-process |
| #187 | f31f152 | `fetchManifest` verifies sibling `manifest.checksum` MD5, asserts `sourceBucket` matches the read bucket, and propagates per-data-file MD5s into `fetchDataKeys` |
| #180 | 4b78a81 | `applyPendingSettings` bails when shutdownCh is closed; persisted config reapplies on next start instead of building new clients during teardown |
| #179 | 4b78a81 | `cachedStats` releases the mutex on cache-hit and uses `singleflight.Do` for misses; errors get a 500ms backoff so a degraded DB isn't replayed instantly |
| #178 | 4b78a81 | `cachedAllFiles` mirrors `cachedStats` (2s TTL keyed on status\|search, singleflight, 500ms error backoff) so a poll loop can't saturate SQLite via `?all=true` |
| #177 | 4b78a81 | `handleTestSource` wraps `source.FromConfig` in a 10s `context.WithTimeout` so a black-holed SMB host can't pin a goroutine for the OS default ~75s |
| #171 | bb9a1c6 | `MarkUploadedMany` checks ctx between rows; `ReconcileZip` checks ctx between chunks so a cancel releases the SQLite write lock at the next chunk boundary |
| #167 | bb9a1c6 | `putResumable` takes runID and tags every emitted `EventUploadProgress`; resume-path emits also throttled to `defaultProgressInterval` |
| #166 | bb9a1c6 | `PutResumable` derives an `uploadCtx` cancelled on first worker error; coalesces errors via `errors.Join`; verifies `len(completed)==expected` before `CompleteMultipart` |
| #168 | bb9a1c6 | `countingReadCloser` tracks per-roundtrip bytes, exposes `Seek` forwarding, and wraps `req.GetBody`; both refund prior bytes via `fn(-n)` so retries don't double-count |
| #132 | 8051920 | `handleDeleteCloudPaths` calls `MarkMissingByPaths` for every successfully-deleted source path so the next existence check doesn't re-upload them |
