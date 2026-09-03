DROP INDEX IF EXISTS idx_releases_obfuscated;
ALTER TABLE releases DROP COLUMN IF EXISTS obfuscated;
