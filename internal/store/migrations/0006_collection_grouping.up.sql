-- Collection grouping: multi-file Usenet posts (e.g. a rar set plus its PAR2)
-- are posted as "[n/total] filename". Recording the collection key and the
-- file counter on each part lets the assembler group the whole collection into
-- a single binary/release instead of one release per file.

-- Stable key shared by every file of one collection (base name + file count).
-- NULL/'' for plain single-file posts, which continue to group by norm_subject.
ALTER TABLE parts ADD COLUMN collection_key TEXT NOT NULL DEFAULT '';
-- This file's 1-based position within the collection (from "[n/total]").
ALTER TABLE parts ADD COLUMN file_number INTEGER NOT NULL DEFAULT 0;
-- Number of files in the collection (the "total" of "[n/total]").
ALTER TABLE parts ADD COLUMN collection_files INTEGER NOT NULL DEFAULT 0;

-- Supports the assembler grouping unassigned collection parts by collection key.
CREATE INDEX idx_parts_collection ON parts (group_id, collection_key, poster)
    WHERE binary_id IS NULL AND collection_key <> '';

-- Binaries gain a collection key so a collection binary is distinct from any
-- per-file binary and so completeness can be judged by distinct files present.
ALTER TABLE binaries ADD COLUMN collection_key TEXT NOT NULL DEFAULT '';
ALTER TABLE binaries ADD COLUMN collection_files INTEGER NOT NULL DEFAULT 0;
