-- Track how many times a release has been through post-processing, so a
-- release whose PAR2/NFO fetch failed transiently can be retried a bounded
-- number of times instead of being marked done (and never retried) or failed
-- forever.
ALTER TABLE releases ADD COLUMN pp_attempts INTEGER NOT NULL DEFAULT 0;

-- Support re-queuing failed-but-retryable releases cheaply.
CREATE INDEX idx_releases_pp_retry ON releases (pp_status, pp_attempts);
