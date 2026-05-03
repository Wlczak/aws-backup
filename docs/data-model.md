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
