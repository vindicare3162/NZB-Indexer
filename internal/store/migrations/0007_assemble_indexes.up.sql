-- Support the assembler's single-file grouping select, which filters
-- unassigned parts that are NOT part of a collection. The existing
-- idx_parts_grouping omits the collection_key predicate, so at scale the
-- planner fell back to a sequential scan + full aggregate. This partial index
-- matches the exact WHERE clause so the grouped scan stays cheap.
CREATE INDEX IF NOT EXISTS idx_parts_grouping_single
    ON parts (group_id, norm_subject, poster)
    WHERE binary_id IS NULL AND collection_key = '' AND norm_subject <> '';
