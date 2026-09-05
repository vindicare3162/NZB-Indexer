-- #127: per-group lag, throughput, and health tracking. Builds on the
-- per-pass scan state from #114 by retaining derived signals across passes so
-- operators can spot stalled, failing, or retention-lagging groups.

-- Last time any pass, a forward pass, and a backfill pass respectively
-- completed successfully. NULL means it has never succeeded in that mode.
ALTER TABLE groups ADD COLUMN last_success_at  TIMESTAMPTZ;
ALTER TABLE groups ADD COLUMN last_forward_at  TIMESTAMPTZ;
ALTER TABLE groups ADD COLUMN last_backfill_at TIMESTAMPTZ;

-- Number of consecutive failed passes since the last success. Reset to 0 on
-- any successful pass; incremented on each failure. Drives failure-health.
ALTER TABLE groups ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0;

-- Exponentially-weighted moving average of ingest throughput in articles per
-- second, updated on each successful pass that pulled articles. 0 = unknown.
ALTER TABLE groups ADD COLUMN throughput_arts_per_sec DOUBLE PRECISION NOT NULL DEFAULT 0;
