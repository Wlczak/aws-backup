# Data Model

The SQLite index models the **state of the S3 bucket**, not the local source. See `CLAUDE.md`.

## Schema (owned by goose migrations)

Migrations live in `internal/db/migrations/*.sql` and are embedded into the binary via `go:embed`. `db.Open` runs them on every start:

- **Fresh DB** — every migration runs from version 0.
- **Pre-existing DB created by the old AutoMigrate path** — detected via "the `files` table exists but `goose_db_version` does not", stamped at the baseline version (`00001_baseline.sql`) so it isn't re-run, then any newer migrations apply normally.
- **Already-migrated DB** — goose is a no-op.

Naming: `NNNNN_short_description.sql` with a `-- +goose Up` block and a matching `-- +goose Down` block. Wrap any statement that contains semicolons (e.g. `CREATE TABLE`) in `-- +goose StatementBegin / StatementEnd` so the parser doesn't split on the inner semicolon.

The current schema below is the cumulative state after all migrations.



```sql
CREATE TABLE files (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    path                TEXT    NOT NULL UNIQUE,
    size                INTEGER NOT NULL,
    mtime               DATETIME NOT NULL,
    md5                 TEXT,
    status              TEXT    NOT NULL DEFAULT 'pending',  -- pending | zipped | uploaded | failed | cloud_only | missing
    zip_name            TEXT,                                -- relative zip path, e.g. "photos/photos_1.zip"
    s3_key              TEXT,                                -- full S3 key
    uploaded_at         DATETIME,
    last_seen_at        DATETIME NOT NULL,
    restore_status      TEXT,                                -- '' | in_progress | restored
    restore_expires_at  DATETIME                             -- when the Glacier restore copy expires
);
CREATE INDEX idx_files_status              ON files(status);
CREATE INDEX idx_files_zip_name            ON files(zip_name);
CREATE INDEX idx_files_last_seen_at        ON files(last_seen_at);
CREATE INDEX idx_files_restore_status      ON files(restore_status);

CREATE TABLE runs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at      DATETIME NOT NULL,
    finished_at     DATETIME,
    status          TEXT NOT NULL DEFAULT 'running',         -- running | completed | failed | cancelled | stopped
    files_scanned   INTEGER DEFAULT 0,
    files_uploaded  INTEGER DEFAULT 0,
    bytes_uploaded  INTEGER DEFAULT 0,
    files_planned   INTEGER DEFAULT 0,         -- planned upload count, written once at upload_plan time
    bytes_planned   INTEGER DEFAULT 0,         -- planned upload byte total, paired with files_planned
    error_message   TEXT
);

CREATE TABLE run_logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     INTEGER NOT NULL REFERENCES runs(id),
    timestamp  DATETIME NOT NULL,
    level      TEXT     NOT NULL,                            -- info | warn | error
    message    TEXT     NOT NULL
);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE multipart_uploads (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id         INTEGER,                                 -- > 0 for individual uploads (mutually exclusive with zip_key)
    zip_key         TEXT,                                    -- non-empty for zip-group uploads
    s3_key          TEXT     NOT NULL,
    upload_id       TEXT     NOT NULL,
    part_size       INTEGER  NOT NULL,
    size            INTEGER  NOT NULL,
    content_sha256  TEXT     NOT NULL,
    started_at      DATETIME NOT NULL,
    last_active_at  DATETIME NOT NULL
);
-- Partial UNIQUE indexes enforce the (file_id | zip_key) natural key
-- so a crash window in UpsertMultipartUpload can't leave duplicates (#173).
CREATE UNIQUE INDEX idx_mpu_file_id_unique ON multipart_uploads(file_id) WHERE file_id > 0;
CREATE UNIQUE INDEX idx_mpu_zip_key_unique ON multipart_uploads(zip_key) WHERE zip_key != '';
```

`stopped` is a graceful-stop terminal status distinct from `cancelled` (force-cancel mid-stream): see #124.

## run_logs retention

`run_logs` is pruned automatically so a chatty run doesn't grow the DB unboundedly. Two passes run after every `FinishRun` (and once at process startup):

- **Per-run cap** (`backup.log_max_per_run`, default 5000) — caps the just-finished run's log row count. When exceeded, the lowest-severity oldest rows are deleted first (info before warn before error) so post-mortem signal survives a chatty info stream. `db.TrimRunLogsForRun`.
- **Age cutoff** (`backup.log_retention_days`, default 30) — drops every log row whose owning run finished more than N days ago. The runs row itself is preserved (the dashboard's history + the row's terminal `error_message` are kept); only the per-line log table is trimmed. `db.TrimRunLogsByAge`.

Either knob set to `0` disables that pass.

## File status transitions

State terms used throughout the codebase:

- `uploaded` - the file exists locally and its object exists in S3.
- `pending` - the file exists locally, but no S3 object has been written yet.
- `cloud only` - the row is stored in SQLite and the object exists in S3, but the file is not currently present on disk. The full sync can recreate or refresh this state from S3.
- `missing` - the row exists in SQLite, but the file is gone locally and there is no recoverable S3 object for it.

```text
pending → zipped → uploaded         (zip group)
pending →          uploaded         (individual file)
pending →  failed                   (upload error; retryable)
{pending,zipped,uploaded,failed} → missing   (file gone from source)
cloud only → pending                (local source reappears during scan)
cloud only → missing                (S3 object deleted)
uploaded → restore_status=in_progress → restored   (Glacier restore lifecycle)
```

`cloud only` is a first-class stored status, not just a label derived from `missing + s3_key`.
`missing` rows are kept until the corresponding S3 object is also deleted - the index models the **bucket**, not the source. See `CLAUDE.md`.
`cloud only` rows are recoverable from S3 and are rebuilt or refreshed by the authoritative sync when the local row is missing.

## Zip naming + sidecar

`{topdir}/{topdir}_{subdir}_{N}.zip` where N is the per-directory counter, seeded from `MAX(DB, S3 listing)`. Sidecar `{zipKey}.index.txt` is uploaded **before** the zip (#95) so partial uploads never leave a zip without an index. Sidecars are uploaded with `Storage.PutStandard` (#125) so they remain instantly listable even when the zip is in Deep Archive.

## Conventions

### Schema lives in goose migrations; GORM owns queries

The split:

- **Schema** — every change ships as a new `internal/db/migrations/NNNNN_*.sql` (Up + Down). `db.Open` runs goose Up; AutoMigrate is **not** called.
- **Queries** — go through GORM models, tags, and the typed query builder. The `gorm:"column:…"` tags are still load-bearing (GORM uses them to map fields ↔ SQL column names). Tags like `gorm:"index"` / `gorm:"uniqueIndex"` are inert under goose; keep them off the model when adding new fields, and document indexes only in the migration.

Don't reach for `db.Exec("CREATE …")` / `tx.Raw(…)` from application code. Raw DDL outside the migrations directory creates a parallel surface to keep in sync.

### Adding a column / table

1. Write a new migration `NNNNN_add_X.sql` with `-- +goose Up` and `-- +goose Down` blocks. Wrap statements containing semicolons (e.g. `CREATE TABLE`) in `-- +goose StatementBegin / StatementEnd`.
2. Update the GORM model struct so queries can reference the new column.
3. Update the schema dump above.
4. If it's a constraint GORM tags can't express (partial UNIQUE, CHECK), put it directly in the migration. See `00002_multipart_unique_indexes.sql` for a worked example (#173).

## Schema-drift behaviour

The app does not gate on a schema version (no "this binary requires DB ≥ vN" check). It opens whatever DB it's pointed at and tries to migrate forward:

- **Fresh DB** — goose runs every migration from version 0.
- **Pre-existing DB created by the old AutoMigrate path** — `Open` detects "the `files` table exists but `goose_db_version` does not" and stamps the baseline as applied without re-running it, then applies any newer migrations.
- **Already-migrated DB at the latest version** — no-op.
- **DB at a version newer than the binary embeds** — goose currently leaves it alone; the binary will run, but anything that depends on a removed column / changed type will fail at query time. There's no downgrade guard.
- **DBs synced from S3 (#143)** are subject to the same rules — the boot-time download lands the file before `Open` runs, so additive drift across binary versions self-heals via goose.
- **Data-format drift inside a column** — SQLite is flexibly typed (TIMESTAMP is TEXT). If different code paths or external tools write `"2026-05-06 14:00:00"` vs `"2026-05-06T14:00:00.123Z"`, both coexist; the pure-Go `glebarez/sqlite` driver only parses one shape, and aggregates like `MIN(timestamp_col)` can return an unparseable string that fails on Scan. When you need to harden a column, the typical pattern is to scan into a `string` first and parse with multiple layouts, or to write a one-shot normalisation pass as a migration.
