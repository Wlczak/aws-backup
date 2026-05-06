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
| — | 4b3b2b9 | Live `copy_progress` SSE event for source→tmp phase |
| — | ea82784 | `S3.MultipartThreshold` config option |
| — | ad3af15 | Settings PUT during a run is queued, not 409'd; applied post-run |
