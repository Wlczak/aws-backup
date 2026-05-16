-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS `download_mirror_snapshots` (
    `download_dir` text PRIMARY KEY,
    `scanned_at` datetime NOT NULL,
    `total_count` integer NOT NULL DEFAULT 0,
    `present_count` integer NOT NULL DEFAULT 0,
    `missing_count` integer NOT NULL DEFAULT 0
);
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS `download_mirror_snapshots`;
