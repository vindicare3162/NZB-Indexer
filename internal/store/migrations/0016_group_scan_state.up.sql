-- #114: expand per-group scan progress and error reporting. Records the outcome
-- of the most recent scan/backfill pass per group so operators can see, per
-- group, when it was last scanned, how much it pulled, how far behind the
-- server head it is, and whether the last pass errored.

-- When the most recent scan/backfill pass for this group completed.
ALTER TABLE groups ADD COLUMN last_scan_at TIMESTAMPTZ;
-- Whether the most recent pass was a backfill (TRUE) or forward scan (FALSE).
ALTER TABLE groups ADD COLUMN last_scan_backfill BOOLEAN NOT NULL DEFAULT FALSE;
-- Articles pulled and parts inserted by the most recent pass.
ALTER TABLE groups ADD COLUMN last_scan_articles BIGINT NOT NULL DEFAULT 0;
ALTER TABLE groups ADD COLUMN last_scan_parts BIGINT NOT NULL DEFAULT 0;
-- The server's high-water article number observed during the most recent
-- forward scan. Combined with last_scanned_high this gives the group's lag
-- (server_high - last_scanned_high). 0 means not yet observed.
ALTER TABLE groups ADD COLUMN server_high BIGINT NOT NULL DEFAULT 0;
-- Error message from the most recent pass, or '' when it succeeded.
ALTER TABLE groups ADD COLUMN last_scan_error TEXT NOT NULL DEFAULT '';
-- When the most recent pass errored (NULL when the last pass succeeded).
ALTER TABLE groups ADD COLUMN last_scan_error_at TIMESTAMPTZ;
