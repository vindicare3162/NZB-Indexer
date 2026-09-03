-- Link each release back to the binary it was promoted from, so the NZB
-- generator can reach the ordered part segments. Nullable because a release
-- may later aggregate multiple binaries (post-processing) or be built from
-- other sources.
ALTER TABLE releases ADD COLUMN binary_id BIGINT REFERENCES binaries (id) ON DELETE SET NULL;

CREATE INDEX idx_releases_binary ON releases (binary_id);
