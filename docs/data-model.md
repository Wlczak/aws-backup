# Data Model

The SQLite index models the **state of the S3 bucket**, not the local source. See `CLAUDE.md`.

## Schema (owned by GORM AutoMigrate)

```sql
CREATE TABLE files (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    path                TEXT    NOT NULL UNIQUE,
    size                INTEGER NOT NULL,
    mtime               DATETIME NOT NULL,
    md5                 TEXT,
    status              TEXT    NOT NULL DEFAULT 'pending',  -- pending | zipped | uploaded | failed | missing
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
```

`stopped` is a graceful-stop terminal status distinct from `cancelled` (force-cancel mid-stream): see #124.

## File status transitions

```text
pending → zipped → uploaded         (zip group)
pending →          uploaded         (individual file)
pending →  failed                   (upload error; retryable)
{pending,zipped,uploaded,failed} → missing   (file gone from source)
uploaded → restore_status=in_progress → restored   (Glacier restore lifecycle)
```

`missing` rows are kept until the corresponding S3 object is also deleted — the index models the **bucket**, not the source. See `CLAUDE.md`.

## Zip naming + sidecar

`{topdir}/{topdir}_{subdir}_{N}.zip` where N is the per-directory counter, seeded from `MAX(DB, S3 listing)`. Sidecar `{zipKey}.index.txt` is uploaded **before** the zip (#95) so partial uploads never leave a zip without an index. Sidecars are uploaded with `Storage.PutStandard` (#125) so they remain instantly listable even when the zip is in Deep Archive.

## Conventions

### Go through GORM, not raw SQL

All schema and queries go through GORM models, tags, and the typed query builder. Don't reach for `db.Exec("CREATE …")` / `tx.Raw(…)` even when GORM tags don't seem to fit — the project convention is that the migration story lives in one place: `AutoMigrate(&Model{})` on `db.Open`. Raw DDL creates a parallel surface to keep in sync with the GORM models and is harder to test and reason about.

When a constraint looks like it needs raw SQL, it usually doesn't:

- **Partial unique indexes** (e.g. `multipart_uploads.file_id` unique only when `> 0`) — GORM supports `uniqueIndex` with a `where:` clause: `gorm:"uniqueIndex:idx_name,where:file_id > 0"`. See `MultipartUpload` for a worked example (#173).
- **Nullable uniqueness** — declare the field as a pointer (`*int64`, `sql.NullString`); SQLite's UNIQUE allows multiple NULLs, so a regular `uniqueIndex` does what you want.
- **Composite indexes** — repeat the `index:name` / `uniqueIndex:name` tag across the participating fields.

If GORM truly can't express it, document the exception in this file and keep the raw SQL minimal and migration-style.

## Schema-drift behaviour

The app does not version the schema. `Open` calls `AutoMigrate` unconditionally and relies on its semantics:

- **New table or new column** — added on the fly; safe across upgrades.
- **Removed column** — left in place forever; no drop.
- **Type change on an existing column** — silently ignored. The column keeps its old SQLite affinity; queries that scan into the new Go type can fail at runtime.
- **No `schema_version` table, no downgrade guard.** A binary opens whatever DB it's pointed at.
- **DBs synced from S3 (#143)** are re-`AutoMigrate`'d on boot, so additive drift across binary versions self-heals; type/value drift does not.
- **Data-format drift inside a column** — SQLite is flexibly typed (TIMESTAMP is TEXT). If different code paths write `"2026-05-06 14:00:00"` vs `"2026-05-06T14:00:00.123Z"`, both coexist; the pure-Go `glebarez/sqlite` driver only parses one shape, and aggregates like `MIN(timestamp_col)` can return an unparseable string that fails on Scan.

When you need to harden this for a column, the typical pattern is to scan into a `string` first and parse with multiple layouts, or to write a one-shot normalisation pass during `Open`.
