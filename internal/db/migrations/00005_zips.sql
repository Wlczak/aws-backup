-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS `zips` (
    `id` integer PRIMARY KEY AUTOINCREMENT,
    `zip_name` text NOT NULL,
    `size` integer NOT NULL DEFAULT 0,
    `md5` text,
    `sha256` text,
    `s3_key` text,
    `uploaded_at` datetime,
    `last_seen_at` datetime NOT NULL
);
-- +goose StatementEnd
CREATE UNIQUE INDEX IF NOT EXISTS `idx_zips_zip_name` ON `zips`(`zip_name`);

ALTER TABLE `files` ADD COLUMN `zip_id` integer;
CREATE INDEX IF NOT EXISTS `idx_files_zip_id` ON `files`(`zip_id`);

-- Backfill one zip row per distinct archive name from legacy file rows.
INSERT OR IGNORE INTO `zips` (`zip_name`, `size`, `md5`, `s3_key`, `uploaded_at`, `last_seen_at`)
SELECT
    `zip_name`,
    COALESCE(SUM(`size`), 0),
    MAX(`md5`),
    MAX(`s3_key`),
    MAX(`uploaded_at`),
    MAX(`last_seen_at`)
FROM `files`
WHERE COALESCE(`zip_name`, '') != ''
GROUP BY `zip_name`;

UPDATE `files`
SET `zip_id` = (
    SELECT `id`
    FROM `zips`
    WHERE `zips`.`zip_name` = `files`.`zip_name`
)
WHERE COALESCE(`zip_name`, '') != '';

-- +goose Down
DROP INDEX IF EXISTS `idx_files_zip_id`;
ALTER TABLE `files` DROP COLUMN `zip_id`;
DROP INDEX IF EXISTS `idx_zips_zip_name`;
DROP TABLE IF EXISTS `zips`;
