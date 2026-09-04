-- #132: standardize post-processing retry policy and permanent-failure handling.
-- Extends the existing bounded-retry (pp_attempts, migration 0008) with
-- exponential backoff, a persisted last error, and an explicit permanent-failure
-- flag so transient failures are retried with backoff while permanent ones
-- (e.g. article expired / not carried) stop retrying immediately.

-- Earliest time a 'failed' release is eligible to be retried again. NULL means
-- eligible immediately (e.g. never failed, or requeued by an operator).
ALTER TABLE releases ADD COLUMN next_retry_at TIMESTAMPTZ;
-- The most recent post-processing failure message (''=none).
ALTER TABLE releases ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
-- TRUE when post-processing failed permanently (a permanent NNTP error or the
-- retry budget was exhausted); such releases are never retried automatically.
ALTER TABLE releases ADD COLUMN pp_permanent BOOLEAN NOT NULL DEFAULT FALSE;

-- Keep the due-release selection cheap: it filters on pp_status, excludes
-- permanent failures, and compares next_retry_at.
CREATE INDEX idx_releases_pp_due ON releases (pp_status, pp_permanent, next_retry_at);
