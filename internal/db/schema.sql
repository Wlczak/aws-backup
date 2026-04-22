CREATE TABLE IF NOT EXISTS files (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    path          TEXT    NOT NULL UNIQUE,
    size          INTEGER NOT NULL,
    mtime         TEXT    NOT NULL,
    md5           TEXT,
    status        TEXT    NOT NULL DEFAULT 'pending',
    zip_name      TEXT,
    s3_key        TEXT,
    uploaded_at   TEXT,
    last_seen_at  TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_files_status       ON files(status);
CREATE INDEX IF NOT EXISTS idx_files_zip_name     ON files(zip_name);
CREATE INDEX IF NOT EXISTS idx_files_last_seen_at ON files(last_seen_at);

CREATE TABLE IF NOT EXISTS runs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at      TEXT    NOT NULL,
    finished_at     TEXT,
    status          TEXT    NOT NULL DEFAULT 'running',
    files_scanned   INTEGER NOT NULL DEFAULT 0,
    files_uploaded  INTEGER NOT NULL DEFAULT 0,
    bytes_uploaded  INTEGER NOT NULL DEFAULT 0,
    error_message   TEXT
);

CREATE INDEX IF NOT EXISTS idx_runs_status     ON runs(status);
CREATE INDEX IF NOT EXISTS idx_runs_started_at ON runs(started_at);

CREATE TABLE IF NOT EXISTS run_logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     INTEGER NOT NULL REFERENCES runs(id),
    timestamp  TEXT    NOT NULL,
    level      TEXT    NOT NULL,
    message    TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_run_logs_run_id ON run_logs(run_id);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
