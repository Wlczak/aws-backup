-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS `run_scan_folders` (
    `run_id` integer NOT NULL,
    `path` text NOT NULL,
    `last_scanned_at` datetime NOT NULL,
    `completed_at` datetime,
    PRIMARY KEY (`run_id`, `path`),
    FOREIGN KEY (`run_id`) REFERENCES `runs`(`id`) ON DELETE CASCADE
);
-- +goose StatementEnd
CREATE INDEX IF NOT EXISTS `idx_run_scan_folders_run_id` ON `run_scan_folders`(`run_id`);
CREATE INDEX IF NOT EXISTS `idx_run_scan_folders_completed_at` ON `run_scan_folders`(`completed_at`);

-- +goose Down
DROP TABLE IF EXISTS `run_scan_folders`;
