DROP INDEX IF EXISTS idx_releases_segments_empty;
ALTER TABLE releases DROP COLUMN IF EXISTS segments;
