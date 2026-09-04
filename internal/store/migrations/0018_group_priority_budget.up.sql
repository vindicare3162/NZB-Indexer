-- #126: per-group scan priority and forward scan budget. Priority lets the
-- worker scan high-value groups first (and, combined with adaptive scheduling,
-- more often); the forward budget lets a group cap how many articles a single
-- forward pass ingests, independent of the global default.

-- Higher priority groups are scanned before lower ones. Default 0.
ALTER TABLE groups ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
-- Per-group override of the global forward-scan per-pass article cap. NULL uses
-- the global default; 0 means unbounded (scan up to the server head).
ALTER TABLE groups ADD COLUMN forward_target_articles BIGINT;

-- Support ordering the active-group scan set by priority (desc) then name.
CREATE INDEX idx_groups_priority ON groups (priority DESC, name);
