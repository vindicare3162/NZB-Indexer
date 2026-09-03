DROP INDEX IF EXISTS idx_releases_binary;
ALTER TABLE releases DROP COLUMN IF EXISTS binary_id;
