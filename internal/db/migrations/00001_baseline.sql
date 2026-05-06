-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS `files` (
    `id` integer PRIMARY KEY AUTOINCREMENT,
    `path` text NOT NULL,
    `size` integer NOT NULL,
    `mtime` datetime NOT NULL,
    `md5` text,
    `status` text NOT NULL DEFAULT "pending",
    `zip_name` text,
    `s3_key` text,
    `uploaded_at` datetime,
    `last_seen_at` datetime NOT NULL,
    `restore_status` text,
    `restore_expires_at` datetime
);
-- +goose StatementEnd
CREATE UNIQUE INDEX IF NOT EXISTS `idx_files_path`             ON `files`(`path`);
CREATE        INDEX IF NOT EXISTS `idx_files_status`           ON `files`(`status`);
CREATE        INDEX IF NOT EXISTS `idx_files_zip_name`         ON `files`(`zip_name`);
CREATE        INDEX IF NOT EXISTS `idx_files_last_seen_at`     ON `files`(`last_seen_at`);
CREATE        INDEX IF NOT EXISTS `idx_files_restore_status`   ON `files`(`restore_status`);

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS `runs` (
    `id` integer PRIMARY KEY AUTOINCREMENT,
    `started_at` datetime NOT NULL,
    `finished_at` datetime,
    `status` text NOT NULL DEFAULT "running",
    `files_scanned` integer NOT NULL DEFAULT 0,
    `files_uploaded` integer NOT NULL DEFAULT 0,
    `bytes_uploaded` integer NOT NULL DEFAULT 0,
    `error_message` text
);
-- +goose StatementEnd
CREATE INDEX IF NOT EXISTS `idx_runs_started_at` ON `runs`(`started_at`);
CREATE INDEX IF NOT EXISTS `idx_runs_status`     ON `runs`(`status`);

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS `run_logs` (
    `id` integer PRIMARY KEY AUTOINCREMENT,
    `run_id` integer NOT NULL,
    `timestamp` datetime NOT NULL,
    `level` text NOT NULL,
    `message` text NOT NULL
);
-- +goose StatementEnd
CREATE INDEX IF NOT EXISTS `idx_run_logs_run_id` ON `run_logs`(`run_id`);

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS `settings` (
    `key` text,
    `value` text NOT NULL,
    PRIMARY KEY (`key`)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS `multipart_uploads` (
    `id` integer PRIMARY KEY AUTOINCREMENT,
    `file_id` integer,
    `zip_key` text,
    `s3_key` text NOT NULL,
    `upload_id` text NOT NULL,
    `part_size` integer NOT NULL,
    `size` integer NOT NULL,
    `content_sha256` text NOT NULL,
    `started_at` datetime NOT NULL,
    `last_active_at` datetime NOT NULL
);
-- +goose StatementEnd
CREATE INDEX IF NOT EXISTS `idx_multipart_uploads_file_id` ON `multipart_uploads`(`file_id`);
CREATE INDEX IF NOT EXISTS `idx_multipart_uploads_zip_key` ON `multipart_uploads`(`zip_key`);

-- +goose Down
DROP TABLE IF EXISTS `multipart_uploads`;
DROP TABLE IF EXISTS `settings`;
DROP TABLE IF EXISTS `run_logs`;
DROP TABLE IF EXISTS `runs`;
DROP TABLE IF EXISTS `files`;
