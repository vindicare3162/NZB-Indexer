DROP INDEX IF EXISTS idx_groups_priority;
ALTER TABLE groups DROP COLUMN forward_target_articles;
ALTER TABLE groups DROP COLUMN priority;
