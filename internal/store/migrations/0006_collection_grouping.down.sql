DROP INDEX IF EXISTS idx_parts_collection;
ALTER TABLE binaries DROP COLUMN IF EXISTS collection_files;
ALTER TABLE binaries DROP COLUMN IF EXISTS collection_key;
ALTER TABLE parts DROP COLUMN IF EXISTS collection_files;
ALTER TABLE parts DROP COLUMN IF EXISTS file_number;
ALTER TABLE parts DROP COLUMN IF EXISTS collection_key;
