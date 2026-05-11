# Recently Closed Issues

Kept as searchable context for past architectural calls. The git log has the full history.

| # | Commit | Summary |
| --- | --- | --- |
| #283 | — | Missing rows without an `s3_key` now stay labeled `missing` instead of being shown as `cloud only` |
| — | — | Full restore scan now HEADs all uploaded/zipped object keys instead of only standalone rows |
| — | — | Download tab now reports restored / in-progress / not-restoring counts and only estimates downloadable rows |
| — | — | Added `/api/restore/download` and a separate Download tab for local restore + MD5 verify flow, with `restore_download_*` SSE progress |
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
| #256 | 6d5bfa2 | `engine.New` honors `ZipMaxBytes < 0` as 'disable cap'; only `0` falls back to the 2 GiB default |
| #250 | 349187b | `config.Validate` rejects `sqs.max_messages=0` so an invalid config fails at save time, not on the first ReceiveMessage |
| #271 | 3c1d6df | `cachedAllFiles` re-emits the underlying DB error on cache hit instead of serving an empty 200 |
| #270 | 6c1c157 | `fetchManifest` rejects manifests whose `sourceBucket` is empty or doesn't equal the read bucket — closes the empty-bypass left by #187 |
| #232 | ad60bd7 | `fetchManifest` rejects manifests with any empty `MD5checksum` so per-data-file verification can't be silently skipped |
| — | — | CI `govulncheck` moved to Go 1.25.10 after GO-2026-4971 / GO-2026-4918 in 1.25.9 |
| — | — | Post-run DB sync now uploads a `VACUUM INTO` snapshot instead of the live `index.db` file |
| #275 | e83c614 | `fetchDataKeys` bounds compressed/uncompressed reads (8 GiB hard cap on gzip stream + manifest-Size compressed cap + 1 MiB per-field cap) — gzip-bomb hardening |
| #274 | e438112 | `Validate` URL-parses `s3.endpoint` / `sqs.queue_url`, requires http/https + non-empty host, and rejects IMDS-class hosts to block credential exfiltration / metadata SSRF |
| #273 | c905d34 | `originGuard` chi middleware on `/api` rejects mismatched-Origin requests so a hostile page can't read /api/events on a loopback dev server |
| #272 | 4325aef | `securityHeaders` middleware sets nosniff, frame-ancestors 'none', no-referrer, and a self+inline CSP on every response |
| #265 | 81bdbb2 | `MarkRestoreInProgress` predicate uses `COALESCE(restore_status,'') != 'restored'` so legacy NULL rows participate |
| #266 | b072465 | Modified-file branch of `UpsertFile`/`UpsertFileBatch` resets `restore_status` + `restore_expires_at` so a re-uploaded file doesn't carry the prior Glacier-restore lifecycle |
| #268 | 55c663b | `PutResumable` errors immediately when the requested object would require >10000 parts, suggesting a minimum `part_size` |
| #267 | 671e743 | Timed-out `handleTestSource` SMB branch spawns a drain goroutine that closes any late-successful dial |
| #269 | 41f31f9 | `runBootDownload`'s 750 ms shutdown wait now `select`s on `ctx.Done()` so SIGINT during boot tears down promptly |
| #260 / #264 | 6c74e50 | `runBackup` honours `signal.NotifyContext`; `fetchDataKeys` PathUnescapes inventory CSV keys so spaces / `+` survive |
| #263 | 81bb494 | Dashboard log only ingests run_log + run lifecycle events (skips per-thread copy/upload progress) |
| #261 | 0b5d884 | `subscribeEvents` debounces onStatus to open↔error transitions only |
| #262 | d804bdd | Logs `<tr>` and Files path `<td>` get role=button + tabindex + Enter/Space activation |
| dashboard-replay | — | persist `files_planned`/`bytes_planned` on `runs`; `sseReplay` synthesises `upload_plan` so a mid-run page reload sees the progress denominator |
| dashboard-zip-count | — | dashboard `uploads` derivation weights each item by `files` from `upload_start`/`upload_complete` so a zip group of N files counts as N completed, not 1 |
| files-lazy-tree | — | new `/api/files/tree` + `/api/files/subtree-ids`; Files page tree view fetches one folder per expand instead of `all=true` dumping the whole index up front |
| restore-batch | — | `RequestRestore` flips covered rows via one bulk `MarkRestoreInProgressMany` (chunked, deduped) instead of one UPDATE per file — a 2000-file zip restore now takes ms, not minutes |
| scan-batch | — | `UpsertFileBatch` partitions via one bulk SELECT then bulk-inserts new rows + bulk-updates `last_seen_at` for unchanged ones, instead of SELECT+INSERT-or-UPDATE per file. Plus `PRAGMA synchronous=NORMAL` paired with WAL to drop the per-commit fsync. Big scans are several × faster |
| estimate-batch | — | `/api/restore/estimate` runs one `SELECT COUNT(*), SUM(size)` with the path filter pushed into SQL instead of paging the whole index into Go and prefix-matching per row. A "select all" estimate on a 1M-row index goes from seconds to ms |
| estimate-depth | — | `RestoreEstimateStats` chunks the OR'd path filter into batches of `sqlChunkSize` and sums across them, so a ~5000-path estimate no longer trips SQLite's `SQLITE_MAX_EXPR_DEPTH` (default 1000) with `Expression tree is too large` |
| restore-request-progress | — | `RequestRestore` emits `restore_request_*` SSE events (one per archive key, throttled every 10) and the Restore page renders a progress bar under "Request retrieval" so a multi-thousand-file thaw shows live `processed/total` instead of looking hung |
| estimate-skip-thawed | — | `RestoreEstimateStats` and `RequestRestore` now exclude rows whose `restore_status` is `in_progress` or `restored` from the count/bytes used for cost + the trigger's S3 calls — re-issuing on those would just extend the AWS expiry and re-bill retrieval. New `already_in_progress_*` / `already_restored_*` (estimate) and `files_skipped_*` (trigger) fields surface what got filtered, and the Restore page shows them under the estimate + result cards |
| log-trim | — | `run_logs` is now auto-trimmed after every `FinishRun` (and once at startup): per-run cap `backup.log_max_per_run` (default 5000, drops lowest-severity oldest first) + age cutoff `backup.log_retention_days` (default 30). Stops a chatty run from growing the DB unboundedly; runs row + terminal `error_message` are preserved so dashboard history is intact |
| remember-completed | — | dashboard seeds `uploads.completed` from `runs.files_uploaded` on every status poll + on `run_start` SSE replay, so a page reload mid-run recovers the completed count instead of resetting the progress bar to 0 |
| #259 / #257 / #252 / #249 | 8f2c7a7 | defer-Close staged file via closure on Put; `uploadProgressCtx` final emit must-emit-once via dedicated atomic; resume verify checks ctx; SQS S3 event key uses PathUnescape |
| #258 | 403898a | `runServe` waits on the SQS consumer goroutine via a done channel before `app.close()` runs |
| #255 / #233 | 8768f89 / d6bfae2 | `applyMu` serialises post-run `applyPendingSettings`, PUT /api/settings, and `handleTriggerRun` so neither a fresh PUT nor a fresh BuildEngine can race the swap |
| #254 | aef5ee5 | `md5AndSHA256File` hashes a staged zip in a single pass via `io.MultiWriter` |
| #253 | 526b99d | `LocalDir.Walk` surfaces a root-level read error so a transient I/O blip doesn't produce an empty walk + MarkMissing avalanche |
| #245 / #244 / #243 / #242 / #241 / #240 | b5107ca | scheduler trigger panic-recovery; `LocalDir.Open` re-checks via EvalSymlinks; `cachedStats` re-emits err on hit; `Restore` maps `RestoreAlreadyInProgress` / `InvalidObjectState` to sentinels; `?page` capped at 100000; `indexWrittenThisRun` flag distinguishes wrote-vs-matched sidecar |
| #234 / #237 / #236 | aa61e09 | bootui builds the error span via createElement + textContent (no innerHTML); pipeline cancel/stop/ctx-Done preserves ind-* tmps; `events.Bus` enforces a 256-subscriber cap and SSE responds 503 when full |
| #235 / #239 / #238 / #251 | d6bfae2 | SMB negotiation+auth+mount under `conn.SetDeadline`; SMB.Walk aborts on session-level errors instead of silently truncating; `/api/sync*` and `/delete-cloud-paths` use `context.WithoutCancel` + server timeout; new `00003_run_logs_fk.sql` migration restores the documented FK |
| #225 / #246 / #227 / #226 / #228 / #230 / #231 / #248 / #247 | 101cfce | `fetchDataKeys` errors on schema-short rows; `MarkRestoreCleared` clears stale in_progress/restored on no-header HEAD; ctx checks in every chunked DB write; Dashboard fix-action errors go through toast; restore preserves `zf.Mode().Perm()`; Logs.svelte uses an AbortController; `PutResumable` derives `partSize` from stored part max; `File.RestoreExpiresAt` flex-parsed in AfterFind from a NullString-backed shadow column; `db.Stats` computes the soonest restore expiry in Go after parseFlexTime |
| #224 / #221 / #222 / #220 / #219 / #218 / #223 | a4fbbe1 | `decodeJSON` wraps the body in `http.MaxBytesReader(16 MiB)`; config dir is 0o700 + chmod-tightened; `/api/sync*` and `/delete-cloud-paths` 409 when a run is in flight; SQS `Run()` does Receive outside `receiveMu`; heartbeat tolerates 3 consecutive failures before giving up; `safeJoin` walks ancestors via EvalSymlinks; `ListLogs` paginates (default 500, max 5000) |
