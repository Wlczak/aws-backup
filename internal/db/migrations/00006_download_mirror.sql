-- +goose Up
-- +goose StatementBegin
ALTER TABLE `files` ADD COLUMN `download_present` integer NOT NULL DEFAULT 0;
ALTER TABLE `files` ADD COLUMN `download_checked_at` datetime;
-- +goose StatementEnd
CREATE INDEX IF NOT EXISTS `idx_files_download_present` ON `files`(`download_present`);

-- +goose Down
DROP INDEX IF EXISTS `idx_files_download_present`;
-- SQLite cannot drop columns in-place; keep the down migration
-- intentionally minimal like the other additive migrations.
