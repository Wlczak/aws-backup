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

## Open Issues (as of 2026-04-25)

### Bug
| # | Title |
|---|---|
| #37 | StoragePrefix goes stale after settings hot-swap |

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
