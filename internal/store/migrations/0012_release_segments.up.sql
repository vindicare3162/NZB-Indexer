-- Durable, denormalized segment storage on the release itself, so a completed
-- release can generate its NZB without the raw `parts` rows still existing.
-- This is the prerequisite for raw-part retention: once a release carries its
-- ordered segments here, its assigned parts are safe to delete.
--
-- Format: a JSON array of objects, ordered for reconstruction:
--   [{"message_id":"...","bytes":123,"number":1,"subject":"..."}, ...]
-- Empty array (the default) marks a legacy release whose segments still live
-- only in `parts`; NZB generation falls back to the parts join for those.
ALTER TABLE releases ADD COLUMN segments JSONB NOT NULL DEFAULT '[]'::jsonb;

-- Partial index to cheaply find legacy releases needing a segment backfill
-- (those with an empty segments array but a linked binary).
CREATE INDEX idx_releases_segments_empty ON releases (id)
    WHERE segments = '[]'::jsonb AND binary_id IS NOT NULL;
