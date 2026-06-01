-- +goose Up
-- Frontend/browser logs are stored separately from run_logs so the
-- dashboard can capture runtime errors, request failures, and console
-- warnings without mixing them into the per-run engine history.
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS `client_logs` (
    `id` integer PRIMARY KEY AUTOINCREMENT,
    `recorded_at` datetime NOT NULL,
    `received_at` datetime NOT NULL,
    `level` text NOT NULL,
    `source` text NOT NULL,
    `message` text NOT NULL,
    `route` text NOT NULL DEFAULT '',
    `url` text NOT NULL DEFAULT '',
    `stack` text NOT NULL DEFAULT '',
    `session_id` text NOT NULL DEFAULT '',
    `context_json` text NOT NULL DEFAULT ''
);
-- +goose StatementEnd
CREATE INDEX IF NOT EXISTS `idx_client_logs_received_at` ON `client_logs`(`received_at`);
CREATE INDEX IF NOT EXISTS `idx_client_logs_level` ON `client_logs`(`level`);

-- +goose Down
DROP TABLE IF EXISTS `client_logs`;
