-- Optional external metadata for a release (matched TV show / movie): title,
-- year, season/episode, a cover/poster URL, and an overview. One row per
-- release; absence means the release has not been enriched. fetched_at is set
-- even on a miss so the enrichment loop does not repeatedly retry permanent
-- non-matches.
CREATE TABLE release_metadata (
    release_id  BIGINT PRIMARY KEY REFERENCES releases (id) ON DELETE CASCADE,
    -- Matched title (show or movie name), empty when no match was found.
    title       TEXT NOT NULL DEFAULT '',
    year        INTEGER,
    season      INTEGER,
    episode     INTEGER,
    -- Provider that produced the match (e.g. 'tvmaze'); empty on a miss.
    source      TEXT NOT NULL DEFAULT '',
    -- Provider's id for the matched title (as text; providers differ).
    external_id TEXT NOT NULL DEFAULT '',
    poster_url  TEXT NOT NULL DEFAULT '',
    overview    TEXT NOT NULL DEFAULT '',
    -- Whether a match was found (distinguishes "looked up, no match" from
    -- "matched but sparse fields").
    matched     BOOLEAN NOT NULL DEFAULT FALSE,
    fetched_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
