-- +goose Up
ALTER TABLE `runs` ADD COLUMN `bytes_scanned` integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE `runs` DROP COLUMN `bytes_scanned`;
