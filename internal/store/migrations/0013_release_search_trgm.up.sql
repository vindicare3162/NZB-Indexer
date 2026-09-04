-- Release search tokenises the query and requires each token to appear in
-- search_name via LIKE '%token%'. The leading wildcard makes the plain btree
-- index on search_name unusable, so broad searches degrade as the catalog
-- grows. A GIN trigram index supports substring/LIKE matching efficiently.
--
-- pg_trgm is a trusted extension (PostgreSQL 13+), so a non-superuser database
-- owner can create it. If your deployment restricts extensions, create it once
-- as a superuser; the index creation below then succeeds on the next migrate.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- GIN trigram index accelerating case-insensitive substring search over the
-- normalized search_name. Matches the LIKE '%token%' predicates used by
-- SearchReleases; the query semantics are unchanged.
CREATE INDEX idx_releases_search_name_trgm ON releases USING gin (search_name gin_trgm_ops);
