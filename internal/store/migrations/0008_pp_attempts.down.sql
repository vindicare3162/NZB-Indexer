DROP INDEX IF EXISTS idx_releases_pp_retry;
ALTER TABLE releases DROP COLUMN IF EXISTS pp_attempts;
