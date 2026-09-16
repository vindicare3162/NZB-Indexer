-- The release builder selects the next binaries to promote with
--   WHERE complete AND NOT released AND total_parts > 0 ORDER BY updated_at LIMIT n
-- Ordering by updated_at let the planner walk idx_binaries_updated and filter,
-- on the assumption that LIMIT would stop it early. That assumption holds only
-- while matching rows are dense. Once most releasable binaries have been
-- promoted — and after the total_parts > 0 requirement excluded the binaries
-- whose completeness was never established (#178) — the matches become sparse
-- and the scan runs on: ~25 million rows discarded to return 1,000, taking ~52s
-- on a loop that repeats every two minutes.
--
-- A partial index on the exact predicate, ordered by the sort column, returns
-- matching rows already in order so LIMIT really can stop early.
CREATE INDEX IF NOT EXISTS idx_binaries_releasable ON binaries (updated_at)
    WHERE complete = TRUE AND released = FALSE AND total_parts > 0;
