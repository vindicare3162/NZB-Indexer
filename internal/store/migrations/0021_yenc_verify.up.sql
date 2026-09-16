-- Many posters strip the "(n/m)" segment counter from the Subject line
-- entirely (a common anti-indexing tactic), leaving total_parts = 0 ("unknown
-- total"). The assembler previously treated the first article seen for such a
-- Subject as a complete single-segment file, when in reality the true
-- part/total/size is only ever present in the yEnc body header of each
-- article (=ybegin part=N total=M size=S name=...), which the Subject-only
-- scanner never inspects. This let single ~700KB fragments of multi-gigabyte
-- files get marked "done" and released as if complete (#178).
--
-- yenc_attempts tracks bounded retries for the body-fetch verification pass
-- that now runs before such an ambiguous part is allowed to complete.
-- Adding a column with a constant default is metadata-only, so this is safe
-- on the (very large) parts table.
ALTER TABLE parts ADD COLUMN IF NOT EXISTS yenc_attempts SMALLINT NOT NULL DEFAULT 0;

-- releases: once a total_parts = 0 release has been checked against its
-- article's real yEnc header, skip re-checking it on every repair pass.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS yenc_verified BOOLEAN NOT NULL DEFAULT FALSE;

-- The indexes below are guarded with IF NOT EXISTS because on an existing
-- large deployment they should be built out-of-band with CREATE INDEX
-- CONCURRENTLY (which cannot run inside a migration's transaction) before
-- this migration is applied; on a fresh/small database they are created here.

-- Candidate discovery for yEnc verification: unassigned, ambiguous (no
-- Subject-declared total), not yet exhausted.
CREATE INDEX IF NOT EXISTS idx_parts_yenc_pending ON parts (id)
    WHERE binary_id IS NULL AND collection_key = '' AND total_parts = 0 AND yenc_attempts < 5;

-- Single-file assembly must only auto-complete a part once its true total is
-- known (declared in the Subject, or filled in by yEnc verification with
-- total=1 for a genuinely standalone post). Ambiguous parts (total_parts = 0)
-- now wait instead of auto-completing, so the assembler's index needs the
-- matching total_parts > 0 predicate. Superseding idx_parts_grouping_single
-- (migration 0007) under a new name keeps this migration's index creation
-- skippable when it was pre-built concurrently.
CREATE INDEX IF NOT EXISTS idx_parts_grouping_single_known ON parts (group_id, norm_subject, poster)
    WHERE binary_id IS NULL AND collection_key = '' AND norm_subject <> '' AND total_parts > 0;

-- Dropping an index is metadata-only, so the now-superseded one goes here.
DROP INDEX IF EXISTS idx_parts_grouping_single;

-- Candidate discovery for the backlog-repair pass: already-released stub
-- releases (total_parts = 0, promoted before this fix existed) not yet
-- checked against their real yEnc header.
CREATE INDEX IF NOT EXISTS idx_releases_yenc_repair ON releases (id)
    WHERE total_parts = 0 AND pp_status = 'done' AND yenc_verified = FALSE;
