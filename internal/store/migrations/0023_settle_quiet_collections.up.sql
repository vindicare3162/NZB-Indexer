-- Posts that declare no file count are grouped by their shared archive base
-- name, so collection_files stays 0 and completeness cannot be measured
-- against a total; SettleQuietCollections instead completes them once they
-- stop receiving parts (#178 follow-up).
--
-- Its predicate is indexed exactly, including the updated_at ordering column.
-- A filter the ordering index does not cover degrades into a full scan as its
-- matches thin out, which has now caused four separate production incidents in
-- this codebase.
CREATE INDEX IF NOT EXISTS idx_binaries_settle_quiet ON binaries (updated_at)
    WHERE complete = FALSE AND released = FALSE
      AND collection_key <> '' AND collection_files = 0;
