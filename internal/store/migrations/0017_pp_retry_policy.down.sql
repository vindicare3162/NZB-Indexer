DROP INDEX IF EXISTS idx_releases_pp_due;
ALTER TABLE releases DROP COLUMN pp_permanent;
ALTER TABLE releases DROP COLUMN last_error;
ALTER TABLE releases DROP COLUMN next_retry_at;
