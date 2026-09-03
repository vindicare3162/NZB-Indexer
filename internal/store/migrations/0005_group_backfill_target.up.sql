-- Per-group backfill targets. When set, these override the global backfill
-- settings for that group: index older posts back to N days ago, or back a
-- maximum of M articles. NULL means "use the global default".
ALTER TABLE groups ADD COLUMN backfill_target_days INTEGER;
ALTER TABLE groups ADD COLUMN backfill_target_articles BIGINT;
