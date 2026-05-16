# Architecture

## Overview

A self-contained Go binary with an embedded Svelte SPA that backs up files from a source (SMB share or local directory) to AWS S3 (Glacier Deep Archive by default). Features:

- Intelligent directory-level zipping with per-zip `.index.txt` sidecars in STANDARD tier
- Local SQLite index that mirrors the **state of the S3 bucket** (not the source — see `CLAUDE.md`)
- S3 reconciliation at run start so a crash between upload and DB commit is idempotent
- Streaming copy-then-upload pipeline with configurable concurrency and per-file resume of cached tmp
- HTTP REST API + SSE progress stream + embedded SPA for monitoring and control
- Mirror download that scans a configured mirror directory, records mirror presence in SQLite, and downloads only missing rows
- Optional SQS integration for tracking S3 Glacier restore-completed events
- Cron scheduler for unattended periodic runs

Module: `github.com/Wlczak/aws-backup`

## Repository Layout

```text
aws-backup/
├── cmd/aws-backup/
│   └── main.go                  # Entry point: subcommands (config, run, serve), appState, applySettings
├── internal/
│   ├── api/
│   │   ├── server.go            # chi router, Deps struct, run-state guards, cfgMu
│   │   ├── spa.go               # Embedded SPA fallback handler
│   │   ├── sse.go               # EventEmitter → HTTP SSE with reconnect replay
│   │   ├── handlers_files.go    # GET/DELETE /api/files, retry, stats (cached)
│   │   ├── handlers_runs.go     # POST /api/runs + cancel/stop/continue, status, post-run db sync
│   │   ├── handlers_download.go # POST /api/download/full mirror job
│   │   ├── handlers_restore.go  # Restore estimate / trigger / sync-status (drains SQS)
│   │   ├── handlers_settings.go # GET/PUT /api/settings (hot-reload + deferred-apply during runs)
│   │   ├── handlers_sync.go     # /api/sync, /sync/full, /sync/delete-cloud-paths
│   │   └── handlers_tests.go    # GET /api/smb/test, /api/s3/test
│   ├── config/
│   │   ├── config.go            # Config tree, Load/Save/Validate/Default/Redacted
│   │   └── path.go              # Platform-specific config dir resolution
│   ├── db/
│   │   ├── db.go                # GORM open, AutoMigrate, busy_timeout, Checkpoint
│   │   ├── files.go             # File model + CRUD: Upsert, Mark*, ListPending, Reconcile, Restore lifecycle
│   │   ├── runs.go              # Run + RunLog models, status constants, AppendLog, UpdateRunStats
│   │   └── settings.go          # Key-value Setting table
│   ├── engine/
│   │   ├── engine.go            # Orchestrator: Run/RunWithID, scan→reconcile→pending→pipeline→finalize
│   │   ├── download.go          # Full mirror download into the configured mirror dir
│   │   ├── zipper.go            # GroupFiles (split by size/dir), CreateZip, ZipName/ZipRelPath
│   │   ├── cloud_index.go       # LoadCloudIndex (parse .index.txt sidecars)
│   │   ├── restore.go           # RestoreToDir (download + zip extraction + path safety)
│   │   ├── buffer.go            # writeBuffer (batched MarkUploaded commits during upload)
│   │   ├── events.go            # Event type constants + EventEmitter func type
│   │   ├── progress.go          # progressReader (throttled per-file upload progress events)
│   │   ├── diskspace.go         # ensureTmpSpace (pre-flight free-space check, 64 MiB margin)
│   │   ├── diskspace_unix.go    # syscall.Statfs implementation
│   │   └── diskspace_windows.go # GetDiskFreeSpaceEx implementation
│   ├── events/
│   │   └── bus.go               # In-process pub/sub fan-out for engine.Event
│   ├── pathutil/
│   │   └── pathutil.go          # HasPrefixPath (exact-match guard), NormalizeS3ListPrefix
│   ├── restore/
│   │   └── sqs_consumer.go      # SQS poller for S3 Glacier restore events → DB
│   ├── scheduler/
│   │   └── scheduler.go         # robfig/cron wrapper, Trigger via HTTP to /api/runs
│   ├── source/
│   │   ├── source.go            # Source interface (Walk, Open, Close)
│   │   ├── factory.go           # NewSource / FromConfig — picks SMB or LocalDir
│   │   ├── localdir.go          # LocalDir source (filepath.Walk)
│   │   ├── smb.go               # SMB source (go-smb2, transparent reconnect)
│   │   └── scan.go              # Scan: upsert in batches, reclassify disappearance by prior state, emit progress
│   └── storage/
│       ├── storage.go           # Storage interface + types (PutResult, HeadResult)
│       ├── s3.go                # AWS SDK v2 backend, multipart, ChecksumSHA256, IfNoneMatch
│       ├── memory.go            # In-process MemStorage for tests
│       ├── progress.go          # progressReader (rate-limited io.Reader wrapper)
│       └── base64.go            # SHA256/MD5 base64 encoding helpers
├── web/                         # Svelte SPA — built and embedded into the binary
│   ├── src/
│   │   ├── App.svelte
│   │   ├── main.ts
│   │   ├── routes/
│   │   │   ├── Dashboard.svelte # Live progress, run controls, copy/upload bars
│   │   │   ├── Files.svelte     # Paginated file index with status filter, retry, delete
│   │   │   ├── Logs.svelte      # Run history + per-run log viewer
│   │   │   ├── Download.svelte  # Download + unzip + MD5 verification into local dir
│   │   │   ├── Restore.svelte   # Glacier estimate, trigger, status sync UI
│   │   │   ├── Settings.svelte  # Top-level Settings shell + sub-route nav
│   │   │   └── settings/        # SourceSettings, StorageSettings, SqsSettings, BackupSettings, ServerSettings
│   │   ├── components/          # FileTreeNode, ProgressBar, StatusBadge, Toaster
│   │   └── lib/                 # api.ts, format.ts, router.ts, selection.ts, toast.ts, tree.ts
│   ├── embed.go                 # go:embed dist/
│   ├── package.json
│   └── vite.config.ts
├── deploy/
│   └── docker-compose.yml       # MinIO + bucket-init for local dev
├── docs/                        # Topical project docs (this directory)
├── docs-guide.md                # Index of docs — read at session start
├── go.mod
├── Makefile
├── CLAUDE.md                    # Session-level invariants (read every session)
└── README.md
```

## Package Responsibilities

| Package | Role |
| --- | --- |
| `cmd/aws-backup` | CLI entry: subcommands `config` (`init`/`path`/`validate`), `run`, `serve`. Owns `appState` (cfg/db/src/store/sched/bus/sqsConsumer), wires `applySettings` for hot-swap, runs scheduler + HTTP server |
| `internal/api` | HTTP layer: chi router, JSON handlers, SSE bridge, in-flight run tracking, cfg mutex shared with cmd via `Deps.ConfigMu` |
| `internal/config` | Config tree (`Source`/`S3`/`SQS`/`Backup`/`Server`), JSON load/save with atomic + fsync, `Validate`, `Redacted`, `Default` |
| `internal/db` | GORM (+ glebarez/sqlite, CGO-free) models and queries. Goose migrations own the schema |
| `internal/engine` | Backup orchestrator + zip grouping + cloud index reader + restore extractor + tmp-space guard |
| `internal/events` | Tiny in-process pub/sub bus that fans `engine.Event` out to SSE subscribers |
| `internal/pathutil` | Canonical path helpers: prefix matching with exact-match guard, S3 list-prefix normalisation |
| `internal/restore` | Long-poll SQS consumer that maps S3 Glacier restore events to `RestoreStatus` rows |
| `internal/scheduler` | robfig/cron wrapper that triggers runs via the HTTP API (so the API serialises them) |
| `internal/source` | `Source` interface + LocalDir/SMB implementations + scan loop with batched DB upserts |
| `internal/storage` | `Storage` interface + S3 backend (multipart, dedup, IfNoneMatch) + MemStorage for tests |

## Key Design Decisions

| Decision | Rationale |
| --- | --- |
| GORM + `glebarez/sqlite` | CGO-free; AutoMigrate owns schema, no schema.sql |
| STANDARD tier for `.index.txt` sidecars | Instant listing of Deep Archive zip contents without restore |
| Zip counter seeded from `MAX(DB, S3)` | Survives DB wipe; combined with `PutIfAbsent` (#116) avoids silent overwrites |
| S3 reconcile before listPending | Idempotent retry after crash between Put and DB commit |
| `writeBuffer` for individual uploads | Batches N MarkUploaded calls into one SQLite transaction |
| `pathutil.HasPrefixPath` shared package | Single canonical implementation with exact-match guard |
| Index models S3, not source | Required for safe `missing` semantics and S3-side reconcile (see `CLAUDE.md`) |
| Download mirror metadata is separate from bucket state | `download_present` / `download_checked_at` track the configured local mirror without changing `uploaded` / `cloud_only` / `missing` semantics |
| Settings deferred-apply during runs | PUT during a run persists + queues; applies post-run instead of 409 |
| Pre-flight `skipIfMatches` (HEAD + SHA256) | Avoids redundant new versions on versioned buckets without DB schema changes (#133) |
| `Storage.PutIfAbsent` via S3 `IfNoneMatch=*` | Atomic dedup at the bucket; engine retries with next counter on collision (#116) |
| Tmp resume via stable `ind-{fileID}` name | Cached copy is reused on upload retry when size+mtime match (#127) |
| Pre-flight `ensureTmpSpace` | Cross-platform free-space check with 64 MiB margin before copy/zip (#138) |
| Idle SQLite connection recycling | `database/sql` keeps the DB handle open but closes the underlying connection after 15 minutes of inactivity, then reopens it on demand |
| Schedule triggers via HTTP, not in-process | The API serialises run launches; cron path uses the same 409 semantics |
| `Deps.ConfigMu` shared with `appState.mu` | Single mutex for cfg reads/writes across both sides (#153) |

## Go Dependencies

```text
github.com/aws/aws-sdk-go-v2 + service/s3 + service/sqs + config + credentials
github.com/go-chi/chi/v5
github.com/glebarez/sqlite (CGO-free SQLite for GORM)
github.com/hirochachacha/go-smb2
github.com/robfig/cron/v3
gorm.io/gorm
modernc.org/sqlite (transitively via glebarez/sqlite)
```
