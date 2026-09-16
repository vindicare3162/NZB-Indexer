-- ListPendingReleases selects due releases ordered by created_at, but no
-- existing index carries created_at alongside the pp_status/pp_permanent
-- filter. At scale (millions of pending releases) this forced a full table
-- scan and sort of the entire releases table on every post-processing cycle
-- (SQLSTATE 57014 statement timeouts, since pending releases can be the vast
-- majority of the table). This partial index lets the pending branch of that
-- selection walk pre-sorted rows and stop after LIMIT instead.
CREATE INDEX IF NOT EXISTS idx_releases_pp_pending_due ON releases (created_at DESC)
    WHERE pp_status = 'pending' AND pp_permanent = FALSE;
