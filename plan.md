# S3 Cold Storage Backup Tool — Project Plan

## Overview

Build a self-contained Go binary with an embedded Svelte SPA that backs up files from an SMB source to AWS S3 (Glacier Deep Archive), with intelligent chunked uploading, a local SQLite index, directory-level zipping, and a local WebUI for monitoring and control.

---

## Project Structure

```
backup-tool/
├── cmd/
│   └── main.go                  # Entry point, wires everything together
├── internal/
│   ├── config/
│   │   └── config.go            # Config struct, load/save from JSON file
│   ├── db/
│   │   ├── db.go                # SQLite connection, migrations
│   │   └── queries.go           # All DB read/write operations
│   ├── engine/
│   │   ├── engine.go            # Core backup orchestrator
│   │   ├── scanner.go           # SMB source scanning
│   │   ├── zipper.go            # Directory-level zip logic
│   │   ├── uploader.go          # S3 upload with multipart + checksum
│   │   └── progress.go          # SSE progress event emitter
│   ├── scheduler/
│   │   └── scheduler.go         # Cron-based scheduling
│   └── api/
│       ├── router.go            # chi router setup
│       ├── handlers.go          # HTTP handlers
│       └── sse.go               # Server-Sent Events stream
├── web/                         # Svelte SPA (built output embedded into binary)
│   ├── src/
│   │   ├── App.svelte
│   │   ├── routes/
│   │   │   ├── Dashboard.svelte
│   │   │   ├── Index.svelte
│   │   │   ├── Logs.svelte
│   │   │   ├── Settings.svelte
│   │   │   └── Restore.svelte
│   │   ├── components/
│   │   │   ├── StatusBadge.svelte
│   │   │   ├── FileTable.svelte
│   │   │   ├── ProgressBar.svelte
│   │   │   └── CostEstimate.svelte
│   │   └── lib/
│   │       └── api.ts           # Typed fetch wrappers for Go API
│   ├── package.json
│   └── vite.config.ts
├── go.mod
├── go.sum
├── Makefile                     # build, dev, embed targets
└── config.json                  # Runtime config (gitignored)
```

---

## Data Model (SQLite)

```sql
-- Tracks every file seen on the SMB source
CREATE TABLE files (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    path          TEXT NOT NULL UNIQUE,   -- full path as seen on SMB share
    size          INTEGER NOT NULL,
    mtime         TEXT NOT NULL,          -- ISO8601
    md5           TEXT,                   -- computed on local tmp copy
    status        TEXT NOT NULL DEFAULT 'pending',
                                          -- pending | zipped | uploaded | failed
    zip_name      TEXT,                   -- which zip archive this file is in (nullable)
    s3_key        TEXT,                   -- S3 object key after upload
    uploaded_at   TEXT,                   -- ISO8601, set after confirmed upload
    last_seen_at  TEXT NOT NULL           -- ISO8601, updated each scan
);

-- One row per backup run
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

-- Log lines attached to a run
CREATE TABLE run_logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     INTEGER NOT NULL REFERENCES runs(id),
    timestamp  TEXT NOT NULL,
    level      TEXT NOT NULL,             -- info | warn | error
    message    TEXT NOT NULL
);

-- App-wide settings (key-value, so settings can be added without migrations)
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
    "host": "",
    "port": 445,
    "username": "",
    "password": "",
    "domain": "",
    "share": "",
    "path": ""
  },
  "s3": {
    "bucket": "",
    "region": "",
    "access_key_id": "",
    "secret_access_key": "",
    "storage_class": "DEEP_ARCHIVE",
    "key_prefix": "backups/"
  },
  "backup": {
    "chunk_size": 10,
    "tmp_dir": "/tmp/backup-tool",
    "schedule": "0 2 * * *",
    "zip_threshold": 50,
    "min_zip_dir_files": 20
  },
  "server": {
    "port": 8080,
    "host": "127.0.0.1"
  }
}
```

---

## Core Engine Logic

### Backup run lifecycle (`engine.go`)

```
1. Create run record in DB (status: running)
2. Scan SMB source → upsert all found files into DB (status: pending if new/changed)
3. Group pending files by top-level directory
4. For each directory group:
   a. If file count >= zip_threshold → zip the group (zipper.go)
   b. Else → upload files individually
5. For each item to upload (zip or individual file):
   a. Copy to tmp dir (chunked, chunk_size files at a time)
   b. Compute MD5 of each file
   c. Upload to S3 with checksum verification (uploader.go)
   d. Verify S3 ETag matches local MD5
   e. Mark files as uploaded in DB + set s3_key and uploaded_at
   f. Delete tmp copies
6. Emit progress events throughout via SSE (progress.go)
7. Update run record (status: completed/failed, stats)
```

### Change detection logic (`scanner.go`)

A file is considered changed (re-queued as pending) if:
- It exists in DB but `mtime` or `size` has changed since last scan
- It does not exist in DB at all (new file)

Files marked `uploaded` that are no longer seen on SMB are marked `status: missing` but never deleted from the index — preserving the historical record.

### Zip strategy (`zipper.go`)

- One zip per top-level directory on the SMB share
- Zip name format: `dirname_YYYYMMDD_HHMMSS.zip`
- If a directory has been zipped before and files have changed:
  - Create a new zip with a new timestamp suffix (don't overwrite)
  - S3 Deep Archive 180-day minimum billing means we never delete old zips — just upload the new version
- Store zip contents manifest in the index (each file row gets `zip_name` set)

### Upload logic (`uploader.go`)

- Use AWS SDK v2 multipart upload manager
- Set `ChecksumAlgorithm: types.ChecksumAlgorithmSha256` on upload
- Verify response ETag before marking uploaded in DB
- Retry up to 3 times on transient errors with exponential backoff
- Emit progress SSE event after each successful upload

---

## API Endpoints (`api/handlers.go`)

```
GET  /api/status              — current run status + last run summary
GET  /api/runs                — paginated list of all runs
GET  /api/runs/:id            — single run detail + logs
POST /api/runs                — trigger a new backup run manually
POST /api/runs/:id/cancel     — cancel a running backup

GET  /api/files               — paginated, searchable file index
                                query params: ?status=&search=&page=&limit=
GET  /api/files/stats         — count by status, total size, etc.

GET  /api/settings            — current config (passwords redacted)
PUT  /api/settings            — update config, validate before saving

GET  /api/restore/estimate    — body: {paths: []}, returns cost estimate
POST /api/restore/trigger     — body: {paths: []}, initiates S3 restore request

GET  /api/events              — SSE stream of live backup progress
```

### SSE event types (`api/sse.go`)

```json
{ "type": "scan_progress",   "data": { "scanned": 1200, "total": 300000 } }
{ "type": "upload_start",    "data": { "key": "backups/photos.zip", "size": 2048000 } }
{ "type": "upload_complete", "data": { "key": "backups/photos.zip", "etag": "..." } }
{ "type": "upload_failed",   "data": { "key": "backups/photos.zip", "error": "..." } }
{ "type": "run_complete",    "data": { "run_id": 42, "status": "completed", "stats": {} } }
```

---

## WebUI Pages

### Dashboard (`Dashboard.svelte`)
- Last run: status, timestamp, files uploaded, bytes transferred
- Next scheduled run countdown
- Quick stats: total files indexed, total size, pending/failed counts
- "Run now" button → POST /api/runs → subscribe to /api/events SSE stream
- Live progress bar and log tail during active run

### Index Browser (`Index.svelte`)
- Searchable, paginated table of all files
- Columns: path, size, mtime, status, zip name, uploaded at
- Filter by status (pending / uploaded / failed / missing)
- Click row → show full details + S3 key

### Run Logs (`Logs.svelte`)
- List of all runs with status badges
- Click run → expandable log viewer with level-colored lines

### Settings (`Settings.svelte`)
- Form for all config.json fields
- SMB connection test button → GET /api/smb/test
- S3 connection test button → GET /api/s3/test
- Schedule input with human-readable preview ("Every day at 2:00 AM")

### Restore Helper (`Restore.svelte`)
- Browse index, multi-select files or directories
- "Estimate cost" → calls /api/restore/estimate → shows breakdown:
  - Request fees (n files × $0.10/1000)
  - Retrieval fees (GB × $0.02)
  - Egress fees (GB × $0.09, first 100GB free)
  - Wait time estimate (12-48h for Deep Archive)
- "Initiate restore" button with confirmation dialog

---

## Go Module Dependencies

```
github.com/aws/aws-sdk-go-v2
github.com/aws/aws-sdk-go-v2/config
github.com/aws/aws-sdk-go-v2/service/s3
github.com/aws/aws-sdk-go-v2/feature/s3/manager
github.com/hirochachacha/go-smb2
github.com/mattn/go-sqlite3
github.com/go-chi/chi/v5
github.com/robfig/cron/v3
```

---

## Makefile Targets

```makefile
build:       # build Svelte, embed into Go binary, compile for current OS
build-win:   # cross-compile for Windows (GOOS=windows)
build-linux: # cross-compile for Linux (GOOS=linux)
dev:         # run Go with hot reload (air) + Vite dev server in parallel
embed:       # build Svelte dist/ only (output goes to web/dist, embedded via go:embed)
migrate:     # run DB migrations against local backup.db
clean:       # remove dist/, tmp/, compiled binaries
```

---

## Cross-platform Notes

- Config file location:
  - Linux: `~/.config/backup-tool/config.json`
  - Windows: `%APPDATA%\backup-tool\config.json`
- DB file lives alongside config
- Tmp dir defaults to OS temp (`os.TempDir()`), overridable in config
- SMB library (`go-smb2`) works on both platforms — no OS-level mount needed
- Single `go build` with `CGO_ENABLED=1` required for sqlite3 (use `modernc.org/sqlite` for CGO-free alternative)

---

## Key Implementation Notes

1. **Never mark a file as uploaded before verifying the S3 ETag** — if the process crashes between upload and DB update, the next run will safely re-upload
2. **Index is the source of truth for incrementals** — never list S3 on incremental runs, only on first setup or manual reconciliation
3. **Zip names include timestamps** — never overwrite an existing S3 object, always create new versioned zips to avoid 180-day Deep Archive billing issues
4. **SSE stream should be cancellable** — if the browser disconnects, clean up the event listener goroutine
5. **Config passwords** — never return raw credentials from the API; redact before JSON response
6. **Graceful shutdown** — on SIGINT/SIGTERM, finish the current chunk upload, mark in-progress files as pending, close DB cleanly
```