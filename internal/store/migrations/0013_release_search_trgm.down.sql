DROP INDEX IF EXISTS idx_releases_search_name_trgm;
-- Leave the pg_trgm extension in place; other objects may depend on it and
-- dropping a shared extension on a down-migration is risky.
