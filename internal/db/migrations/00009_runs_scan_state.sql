-- +goose Up
ALTER TABLE `runs` ADD COLUMN `scan_paused` integer NOT NULL DEFAULT 0;
ALTER TABLE `runs` ADD COLUMN `scan_complete` integer NOT NULL DEFAULT 0;

-- +goose Down
-- SQLite can't drop columns cleanly without rebuilding the table; keep
-- the down migration explicit but no-op to avoid destructive table rewrites.
