# aws-backup — Project Plan

> **Read this file at the start of every session.** It is the authoritative record of project state, architecture decisions, issue handling workflow, and open work.

---

## Overview

A self-contained Go binary with an embedded Svelte SPA that backs up files from an SMB (or local) source to AWS S3 (Glacier Deep Archive). Features: intelligent directory-level zipping, a local SQLite index, S3 state reconciliation, a REST API, SSE live events, and a web UI for monitoring and control.

Module: `github.com/Wlczak/aws-backup`

---

## Actual File Structure

```
aws-backup/
├── cmd/aws-backup/
│   └── main.go                  # Entry point, scheduler, discardResponse
├── internal/
│   ├── api/
│   │   ├── server.go            # chi router, Server struct, runMu/currentRun
│   │   ├── spa.go               # embedded SPA handler
│   │   ├── sse.go               # SSE event stream
│   │   ├── handlers_files.go    # GET /api/files, /api/files/stats
│   │   ├── handlers_runs.go     # POST /api/runs, cancel, status
│   │   ├── handlers_restore.go  # GET /api/restore/estimate, POST /api/restore/trigger, POST /api/restore/do
│   │   ├── handlers_settings.go # GET/PUT /api/settings
│   │   ├── handlers_sync.go     # POST /api/sync (full sync / desync fix actions)
│   │   └── handlers_tests.go    # GET /api/test/smb, /api/test/s3
│   ├── config/
│   │   ├── config.go            # Config struct, Load/Save/Validate/Default
│   │   └── path.go              # Platform config path resolution
│   ├── db/
│   │   ├── db.go                # SQLite open, schema migrations
│   │   ├── files.go             # File CRUD, UpsertFile, MarkUploaded, MarkMissing, ReconcileZip, …
│   │   ├── runs.go              # Run CRUD, AppendLog, UpdateRunStats
│   │   └── settings.go          # Key-value settings table
│   ├── engine/
│   │   ├── engine.go            # Backup orchestrator: scan→reconcile→pending→zip/upload
│   │   ├── zipper.go            # GroupFiles, CreateZip, ZipRelPath
│   │   ├── cloud_index.go       # LoadCloudIndex, readIndexInto (S3 index sidecar reader)
│   │   ├── restore.go           # RestoreToDir
│   │   ├── buffer.go            # writeBuffer (batched DB writes during upload)
│   │   └── events.go            # Event types and EventEmitter interface
│   ├── events/
│   │   └── bus.go               # In-process pub/sub event bus
│   ├── pathutil/
│   │   └── pathutil.go          # HasPrefixPath (shared, canonical, with exact-match guard)
│   ├── scheduler/
│   │   └── scheduler.go         # Cron scheduler wrapping robfig/cron
│   ├── source/
│   │   ├── source.go            # Source interface (Walk, Open, Close)
│   │   ├── factory.go           # NewSource — picks SMB or LocalDir
│   │   ├── localdir.go          # LocalDir source (dev/test)
│   │   ├── smb.go               # SMB source via go-smb2
│   │   └── scan.go              # source.Scan — upserts files, marks missing
│   └── storage/
│       ├── storage.go           # Storage interface + types
│       ├── s3.go                # Real S3 backend (aws-sdk-go-v2)
│       ├── memory.go            # MemStorage — in-process fake for tests
│       └── base64.go            # Base64 checksum helpers
├── web/                         # Svelte SPA (built → embedded into binary)
│   ├── src/
│   │   ├── App.svelte
│   │   ├── routes/              # Dashboard, Files, Logs, Settings, Restore
│   │   ├── components/
│   │   └── lib/api.ts           # Typed fetch wrappers
│   ├── embed.go                 # go:embed directive
│   ├── package.json
│   └── vite.config.ts
├── deploy/
│   └── docker-compose.yml       # MinIO dev environment
├── go.mod
├── Makefile
├── plan.md                      # ← this file
└── README.md
```

---

## Data Model (SQLite)

```sql
CREATE TABLE files (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    path          TEXT    NOT NULL UNIQUE,
    size          INTEGER NOT NULL,
    mtime         TEXT    NOT NULL,          -- ISO8601
    md5           TEXT,
    status        TEXT    NOT NULL DEFAULT 'pending',
                                             -- pending | zipped | uploaded | failed | missing
    zip_name      TEXT,                      -- relative zip path (e.g. "photos/photos_1.zip")
    s3_key        TEXT,                      -- full S3 key
    uploaded_at   TEXT,
    last_seen_at  TEXT    NOT NULL
);

CREATE TABLE runs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at      TEXT NOT NULL,
    finished_at     TEXT,
    status          TEXT NOT NULL DEFAULT 'running',
                                             -- running | completed | failed | cancelled
    files_scanned   INTEGER DEFAULT 0,
    files_uploaded  INTEGER DEFAULT 0,
    bytes_uploaded  INTEGER DEFAULT 0,
    error_message   TEXT
);

CREATE TABLE run_logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     INTEGER NOT NULL REFERENCES runs(id),
    timestamp  TEXT    NOT NULL,
    level      TEXT    NOT NULL,             -- info | warn | error
    message    TEXT    NOT NULL
);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
```

---

## Config Schema (config.json)

```json
{
  "smb": {
    "host": "", "port": 445,
    "username": "", "password": "", "domain": "",
    "share": "", "path": ""
  },
  "s3": {
    "bucket": "", "region": "",
    "access_key_id": "", "secret_access_key": "",
    "storage_class": "DEEP_ARCHIVE",
    "key_prefix": "backups/",
    "endpoint": "",
    "use_path_style": false
  },
  "backup": {
    "chunk_size": 10,
    "tmp_dir": "",
    "schedule": "0 2 * * *",
    "zip_threshold": 50,
    "zip_max_bytes": 0,
    "enable_zip_index": true,
    "retry_failed": true
  },
  "server": {
    "port": 8080,
    "host": "127.0.0.1"
  }
}
```

---

## Engine Backup Lifecycle

```
1.  Create run row (DB)
2.  Scan source → upsert files, mark missing (source.Scan)
3.  List all S3 objects under KeyPrefix (single round-trip, reused below)
4.  reconcileFromS3 — read every .index.txt sidecar whose .zip exists in S3;
    mark files still pending/zipped/failed in DB as uploaded
    (closes crash window between successful S3 put and DB commit)
5.  listPending → group files (GroupFiles)
6.  Seed per-directory zip counters from DB + S3 listing (take max)
7.  For each group:
    a. Zip group  → CreateZip → Storage.Put → SetZipName → MarkUploadedBatch
                 → PutStandard(.index.txt sidecar)
    b. Individual → copyAndHash → Storage.Put → writeBuffer (batched commit)
8.  Finalize run row
```

### Status transitions

```
pending → zipped → uploaded
pending →          uploaded   (individual)
pending →  failed             (upload error)
uploaded → missing            (file gone from source on next scan)
zipped  → missing             (file gone from source — MarkMissing covers both)
```

### Zip naming

Format: `{topdir}/{topdir}_{subdir}_{N}.zip` — counter N is seeded from DB **and** S3 so it survives DB loss. Index sidecar: `{zipKey}.index.txt` in STANDARD tier.

---

## API Endpoints

```
GET  /api/status                — current + last run summary
GET  /api/runs                  — paginated run list
GET  /api/runs/:id              — run detail + logs
POST /api/runs                  — trigger run  {mode: full|scan|upload, paths: []}
POST /api/runs/:id/cancel       — cancel in-flight run

GET  /api/files                 — paginated file index  ?status=&search=&page=&limit=
GET  /api/files/stats           — counts by status + total size

GET  /api/settings              — config (passwords redacted)
PUT  /api/settings              — update + hot-reload config

POST /api/sync                  — full-sync / desync-fix actions
GET  /api/restore/estimate      — cost estimate  {paths: []}
POST /api/restore/trigger       — initiate S3 restore request
POST /api/restore/do            — download restored files to local path

GET  /api/test/smb              — SMB connectivity test
GET  /api/test/s3               — S3 connectivity test

GET  /api/events                — SSE live backup progress stream
GET  /*                         — embedded Svelte SPA
```

---

## Key Design Decisions

| Decision | Rationale |
|---|---|
| GORM + `github.com/glebarez/sqlite` | CGO-free ORM; AutoMigrate owns schema, no schema.sql |
| STANDARD tier for `.index.txt` | Instant listing of zip contents without Glacier restore |
| Zip counter seeded from DB **and** S3 | Survives DB wipe; no silent overwrites |
| S3 reconcile before listPending | Idempotent retry after crash between S3 put and DB commit |
| `writeBuffer` for individual uploads | Batches N MarkUploaded calls into one SQLite transaction |
| `pathutil.HasPrefixPath` shared package | Single canonical implementation with exact-match guard |

---

## Issue Handling Workflow

When fixing a GitHub issue:

1. Read the issue: `gh issue view <N> --json title,body,labels,comments`
2. Implement the fix with a test where applicable
3. Build + run relevant tests: `go build ./... && go test ./...`
4. Commit with the issue number in the message title, e.g. `Fix foo (#N)`
5. Close the issue and leave a comment citing the commit SHA:
   ```
   gh issue close <N>
   gh issue comment <N> --body "Fixed in <SHA> — <one-line summary>"
   ```
6. Update this plan if the fix changes architecture or workflow

---

## Open Issues (as of 2026-04-26)

### Bug
| # | Title |
|---|---|
| (none open) | All 21 newly-filed bug issues resolved on 2026-04-26 |

### Bug (formerly open, now closed)
| # | Title |
|---|---|
| #37 | StoragePrefix goes stale after settings hot-swap (now covered by #91 fix) |

### Security
| # | Title |
|---|---|
| #35 | Restore API writes to arbitrary absolute filesystem paths |
| #36 | No authentication on any API endpoint |
| #45 | SMB source Open() lacks path-traversal guard |
| #55 | index.db created with world-readable permissions |

### Performance
| # | Title |
|---|---|
| #40 | Stats cache stampede under concurrent dashboard polls |
| #46 | SQLite busy_timeout not configured |
| #47 | putWithClass ignores size parameter — ContentLength not set on S3 uploads |

### Quality
| # | Title |
|---|---|
| #41 | MinZipDirFiles config field is defined but never used |
| #42 | handleTestStorage permanently blocks real AWS S3 |
| #49 | joinKey function duplicated across api and engine packages |
| #50 | req variable shadowed in handleRestoreEstimate loop |
| #51 | Hand-rolled itoa in source/scan.go should use strconv |

### Convention
| # | Title |
|---|---|
| #52 | handleGetRun returns 404 for all DB errors, not just missing rows |
| #53 | ListPending WHERE clause uses bare OR without parentheses |
| #54 | Docker Compose uses unpinned 'latest' image tags for MinIO |

---

## Recently Closed Issues

| # | Commit | Summary |
|---|---|---|
| #38 | db02540 | `MarkMissing` now includes `zipped` rows |
| #39 | f1880af | `currentRun` leak + `discardResponse` header on panic |
| #43 | b223d84 | Reconcile DB from S3 zip indexes at backup start |
| #44 | de9b06e | `hasPrefixPath` extracted to shared `pathutil` package |
| #87 | ac6714a | `HasPrefixPath` tolerates trailing slash on prefix |
| #92 | ac6714a | Upload loop uses partial `UpdateUploadStats`, preserves scan count |
| #93 | ac6714a | `runWithID` returns real `runErr` even when `FinishRun` fails |
| #97 | ac6714a | `Storage.List` callers normalize prefix via `pathutil.NormalizeS3ListPrefix` |
| #100 | ac6714a | `handleTriggerRun` goroutine defers `cancel()` to release run context |
| #82 | 031f359 | `MarkMissing` now includes `pending` and `failed` rows |
| #86 | 031f359 | `downloadDBFromS3` writes atomically via `.part` + rename |
| #89 | 031f359 | `handlePutSettings` surfaces rollback errors instead of dropping them |
| #90 | 031f359 | `source.Scan` cancels walker via `WithCancelCause` on flush failure |
| #96 | 031f359 | `LoadCloudIndex` skips `.index.txt` sidecars whose zip is missing |
| #58 | 8589466 | S3 `putWithClass` populates `PutResult.Size` |
| #59 | 8589466 | S3 `putWithClass` omits `ContentLength` when size is unknown |
| #60 | 8589466 | CLI `runBackup` passes `MinZipDirFiles` to `engine.New` |
| #95 | 8589466 | `processZipGroup` uploads `.index.txt` sidecar before the zip |
| #98 | 8589466 | `handleRestoreEstimate` filters by `uploaded`/`zipped` status |
| #61 | 9d88159 | `subscribeEvents` accepts onStatus callback for connect/disconnect signals |
| #62 | 9d88159 | S3 `Put` routes >5 GiB / unknown-size bodies through multipart uploader |
| #81 | 9d88159 | Per-file upload errors skipped; only all-groups-failed marks run failed |
| #83 | 9d88159 | `writeBuffer.flush` requeues on error; inline flushes log + retry |
| #84 | 9d88159 | `MarkZipUploadedBatch` collapses zip+upload DB writes into one txn |
| #63 | e96f640 | `applySettings` closes swapped-out source/storage; PUT refuses while run in flight |
| #85 | e96f640 | `Server.Shutdown` cancels run + waits on `runWg` before DB/storage teardown |
| #88 | e96f640 | SMB.Open transparently re-dials + remounts on session-level errors |
| #91 | e96f640 | `StoragePrefix` accessed via `cfgMu`-guarded `storagePrefix()` |
| #94 | e96f640 | `deps.Config` accessed via `cfgMu`-guarded `snapshotConfig()` / `updateConfig()` |
| #101 | ebfb6c5 | `config.Save` fsyncs tmp file and parent dir for crash-safe atomic write |
| #102 | ebfb6c5 | LocalDir / SMB walkers log + skip per-entry errors instead of aborting whole walk |
| #103 | ebfb6c5 | `ReconcileZip` requires `uploaded_at IS NULL` to avoid rebinding modified files to stale zip |
| #104 | ebfb6c5 | SMB.Walk/Close snapshot/lock `s.share` to prevent torn read vs reconnectLocked |
| #105 | ebfb6c5 | `reconnectLocked` swaps connection state only on full success; `Open` re-attempts when share is nil |
| #106 | ebfb6c5 | `source.Scan` prefers `flushErr` and `context.Cause(scanCtx)` over masked `context.Canceled` |
| #107 | ebfb6c5 | `isSMBSessionError` covers timeouts, network-name-deleted, no-route-to-host, etc. |
| #108 | ebfb6c5 | `runServe` calls `sched.Stop` explicitly before `httpSrv.Shutdown` to drain in-flight tick |
| #109 | ebfb6c5 | S3 multipart uploads request composed full-object SHA256 via `ChecksumAlgorithm` |
| #110 | ebfb6c5 | DB chunk-loop helpers (`MarkUploadedBatch`, `SetZipName`, `MarkPendingByIDs`, `DeleteFiles`, `markPendingByColumn`) wrap chunks in a single transaction |
| #111 | ebfb6c5 | `config.Validate` rejects cron expressions that never fire (Feb 30, etc.) |
| #112 | ebfb6c5 | `LocalDir.Open` boundary check tolerates root that already ends in `os.PathSeparator` |
| #113 | ebfb6c5 | Walkers reject RelPaths containing NUL/CR/LF |
| #114 | ebfb6c5 | Scheduler trigger surfaces non-success HTTP statuses (excluding 409) as errors |
| #115 | ebfb6c5 | `applySettings` pre-validates the cron expression before any source/storage swap |
| #116 | ebfb6c5 | `Storage.PutIfAbsent` uses S3 `IfNoneMatch=*`; engine retries zip upload at next counter slot on collision |
| #117 | ebfb6c5 | `handleStatus` returns 500 on DB errors instead of 200 + empty body |
| #118 | ebfb6c5 | `runWithID` drains writeBuffer before `FinishRun` / `EventRunComplete` so files_uploaded matches DB |
| #119 | ebfb6c5 | Upload-phase setup reclassifies `context.Canceled` / `DeadlineExceeded` as `RunCancelled` |
| #120 | ebfb6c5 | `reconcileFromS3` logs and skips per-sidecar read/DB errors instead of aborting the run |
| #121 | ebfb6c5 | `reconcileFromS3` deletes orphan `.index.txt` sidecars whose backing zip is missing |

---

## Go Dependencies

```
github.com/aws/aws-sdk-go-v2 + service/s3 + config + credentials
github.com/go-chi/chi/v5
github.com/hirochachacha/go-smb2
github.com/robfig/cron/v3
modernc.org/sqlite
```

---

## Makefile Targets

```makefile
build        # build Svelte, embed, compile for current OS
build-win    # cross-compile GOOS=windows
build-linux  # cross-compile GOOS=linux
dev          # Go hot-reload (air) + Vite dev server in parallel
embed        # build Svelte dist/ only
clean        # remove dist/, tmp/, binaries
```
