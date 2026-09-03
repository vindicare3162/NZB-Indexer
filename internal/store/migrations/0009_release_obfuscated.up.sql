-- Flag releases whose name is still obfuscated (random hex/base64 with no real
-- words), so search can exclude unusable junk by default. Set at build time and
-- cleared when post-processing recovers a real name.
ALTER TABLE releases ADD COLUMN obfuscated BOOLEAN NOT NULL DEFAULT FALSE;

-- Support the default search filter (obfuscated = false) alongside recency.
CREATE INDEX idx_releases_obfuscated ON releases (obfuscated, posted_at DESC);
