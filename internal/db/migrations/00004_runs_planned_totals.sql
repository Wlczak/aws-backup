-- +goose Up
-- Persist the planned upload total alongside the live FilesUploaded
-- counter so a mid-run page reload can recover the progress-bar
-- denominator from the DB (the upload_plan SSE event only fires once,
-- at plan time, and is not replayed otherwise).
ALTER TABLE `runs` ADD COLUMN `files_planned` integer NOT NULL DEFAULT 0;
ALTER TABLE `runs` ADD COLUMN `bytes_planned` integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE `runs` DROP COLUMN `bytes_planned`;
ALTER TABLE `runs` DROP COLUMN `files_planned`;
