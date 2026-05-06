-- +goose Up
-- Replace the plain (file_id) / (zip_key) indexes with partial UNIQUE
-- indexes so the natural-key invariant is enforced at the schema level.
-- Without this, only application-level discipline in
-- UpsertMultipartUpload's delete-then-create kept rows from
-- duplicating, and a crash window or any future concurrent writer
-- could leave two rows with the same discriminator. (#173)
DROP INDEX IF EXISTS `idx_multipart_uploads_file_id`;
DROP INDEX IF EXISTS `idx_multipart_uploads_zip_key`;
CREATE UNIQUE INDEX IF NOT EXISTS `idx_mpu_file_id_unique`
    ON `multipart_uploads`(`file_id`)
    WHERE `file_id` > 0;
CREATE UNIQUE INDEX IF NOT EXISTS `idx_mpu_zip_key_unique`
    ON `multipart_uploads`(`zip_key`)
    WHERE `zip_key` != '';

-- +goose Down
DROP INDEX IF EXISTS `idx_mpu_zip_key_unique`;
DROP INDEX IF EXISTS `idx_mpu_file_id_unique`;
CREATE INDEX IF NOT EXISTS `idx_multipart_uploads_file_id` ON `multipart_uploads`(`file_id`);
CREATE INDEX IF NOT EXISTS `idx_multipart_uploads_zip_key` ON `multipart_uploads`(`zip_key`);
