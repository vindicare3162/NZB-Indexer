DROP INDEX IF EXISTS idx_releases_yenc_repair;
ALTER TABLE releases DROP COLUMN IF EXISTS yenc_verified;

CREATE INDEX IF NOT EXISTS idx_parts_grouping_single ON parts (group_id, norm_subject, poster)
    WHERE binary_id IS NULL AND collection_key = '' AND norm_subject <> '';
DROP INDEX IF EXISTS idx_parts_grouping_single_known;

DROP INDEX IF EXISTS idx_parts_yenc_pending;
ALTER TABLE parts DROP COLUMN IF EXISTS yenc_attempts;
