-- +goose Up
-- Recreate run_logs with the FK on run_id that the data-model doc has
-- always advertised. Without it, foreign_keys=ON had nothing to enforce
-- on this table and a future DELETE FROM runs would silently leave
-- orphan log rows. (#251)
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS `run_logs_new` (
    `id` integer PRIMARY KEY AUTOINCREMENT,
    `run_id` integer NOT NULL REFERENCES `runs`(`id`) ON DELETE CASCADE,
    `timestamp` datetime NOT NULL,
    `level` text NOT NULL,
    `message` text NOT NULL
);
-- +goose StatementEnd
INSERT INTO `run_logs_new`(`id`, `run_id`, `timestamp`, `level`, `message`)
    SELECT `id`, `run_id`, `timestamp`, `level`, `message` FROM `run_logs`
    WHERE `run_id` IN (SELECT `id` FROM `runs`);
DROP TABLE `run_logs`;
ALTER TABLE `run_logs_new` RENAME TO `run_logs`;
CREATE INDEX IF NOT EXISTS `idx_run_logs_run_id` ON `run_logs`(`run_id`);

-- +goose Down
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS `run_logs_old` (
    `id` integer PRIMARY KEY AUTOINCREMENT,
    `run_id` integer NOT NULL,
    `timestamp` datetime NOT NULL,
    `level` text NOT NULL,
    `message` text NOT NULL
);
-- +goose StatementEnd
INSERT INTO `run_logs_old`(`id`, `run_id`, `timestamp`, `level`, `message`)
    SELECT `id`, `run_id`, `timestamp`, `level`, `message` FROM `run_logs`;
DROP TABLE `run_logs`;
ALTER TABLE `run_logs_old` RENAME TO `run_logs`;
CREATE INDEX IF NOT EXISTS `idx_run_logs_run_id` ON `run_logs`(`run_id`);
