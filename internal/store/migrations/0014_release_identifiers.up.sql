-- Normalized external identifiers for releases (IMDb, TVDB, TMDB, ...), so
-- integrations can search and match by provider ID rather than only by
-- release-name text. A release may carry several identifiers from different
-- providers; each (source, identifier) is unique per release.
CREATE TABLE release_identifiers (
    id          BIGSERIAL PRIMARY KEY,
    release_id  BIGINT NOT NULL REFERENCES releases (id) ON DELETE CASCADE,
    -- Provider key, lower-cased and normalized (e.g. 'imdb', 'tvdb', 'tmdb').
    source      TEXT NOT NULL,
    -- Normalized identifier value in the source's canonical form
    -- (e.g. IMDb 'tt0111161', TVDB/TMDB decimal digits).
    identifier  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (release_id, source, identifier)
);

-- Look up releases by a provider id (source, identifier) for search filtering.
CREATE INDEX idx_release_identifiers_lookup ON release_identifiers (source, identifier);
CREATE INDEX idx_release_identifiers_release ON release_identifiers (release_id);
