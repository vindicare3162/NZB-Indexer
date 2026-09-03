-- goindex initial schema.
-- Pipeline flow: parts (raw header rows) -> binaries (grouped parts) ->
-- releases (searchable items) with release_files (per-file detail). Newznab
-- categories, users, and api_keys support the API/auth layers.

-- Newznab-style categories. Parent categories have parent_id = NULL; leaf
-- categories reference their parent. IDs follow the Newznab convention
-- (e.g. 2000 Movies, 2040 Movies/HD).
CREATE TABLE categories (
    id          INTEGER PRIMARY KEY,
    parent_id   INTEGER REFERENCES categories (id) ON DELETE SET NULL,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_categories_parent ON categories (parent_id);

-- Newsgroups being indexed, with per-group scan position tracking.
CREATE TABLE groups (
    id                  BIGSERIAL PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    active              BOOLEAN NOT NULL DEFAULT TRUE,
    -- Highest article number ingested so far (forward scan watermark).
    last_scanned_high   BIGINT NOT NULL DEFAULT 0,
    -- Lowest article number ingested so far (backfill watermark). 0 means
    -- no backfill has run yet.
    backfill_low        BIGINT NOT NULL DEFAULT 0,
    -- Whether backfill has reached the configured depth/date.
    backfill_complete   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_groups_active ON groups (active);

-- Raw article header rows harvested via XOVER. A "part" is one article that
-- belongs to a larger multi-part binary. part_number/total_parts are parsed
-- from the subject where possible (0 when absent).
CREATE TABLE parts (
    id              BIGSERIAL PRIMARY KEY,
    group_id        BIGINT NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    -- NNTP article number within the group.
    article_number  BIGINT NOT NULL,
    -- Global RFC 3977 message-id (without angle brackets).
    message_id      TEXT NOT NULL,
    subject         TEXT NOT NULL,
    poster          TEXT NOT NULL DEFAULT '',
    posted_at       TIMESTAMPTZ,
    bytes           BIGINT NOT NULL DEFAULT 0,
    part_number     INTEGER NOT NULL DEFAULT 0,
    total_parts     INTEGER NOT NULL DEFAULT 0,
    -- Normalized subject with the volatile part counter stripped, used to
    -- group parts of the same binary together.
    norm_subject    TEXT NOT NULL DEFAULT '',
    -- Set once the assembler folds this part into a binary.
    binary_id       BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (group_id, article_number)
);

CREATE INDEX idx_parts_message_id ON parts (message_id);
CREATE INDEX idx_parts_binary ON parts (binary_id);
-- Supports the assembler grouping unassigned parts by normalized subject.
CREATE INDEX idx_parts_grouping ON parts (group_id, norm_subject, poster)
    WHERE binary_id IS NULL;

-- A binary is a collection of parts that together form one posted file set
-- (before release-level naming/categorization).
CREATE TABLE binaries (
    id              BIGSERIAL PRIMARY KEY,
    group_id        BIGINT NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    norm_subject    TEXT NOT NULL,
    poster          TEXT NOT NULL DEFAULT '',
    total_parts     INTEGER NOT NULL DEFAULT 0,
    collected_parts INTEGER NOT NULL DEFAULT 0,
    total_bytes     BIGINT NOT NULL DEFAULT 0,
    posted_at       TIMESTAMPTZ,
    complete        BOOLEAN NOT NULL DEFAULT FALSE,
    -- Set once the release builder promotes this binary to a release.
    released        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (group_id, norm_subject, poster)
);

CREATE INDEX idx_binaries_complete ON binaries (complete, released);
CREATE INDEX idx_binaries_updated ON binaries (updated_at);

-- A release is the searchable, categorized, human-named item exposed via the
-- APIs. release_hash is a stable fingerprint used for deduplication.
CREATE TABLE releases (
    id              BIGSERIAL PRIMARY KEY,
    -- Public identifier used in Newznab GUIDs / NZB download URLs.
    guid            TEXT NOT NULL UNIQUE,
    name            TEXT NOT NULL,
    -- Original (possibly obfuscated) subject the name was derived from.
    original_subject TEXT NOT NULL DEFAULT '',
    search_name     TEXT NOT NULL DEFAULT '',
    category_id     INTEGER REFERENCES categories (id) ON DELETE SET NULL,
    group_id        BIGINT REFERENCES groups (id) ON DELETE SET NULL,
    poster          TEXT NOT NULL DEFAULT '',
    total_parts     INTEGER NOT NULL DEFAULT 0,
    size_bytes      BIGINT NOT NULL DEFAULT 0,
    posted_at       TIMESTAMPTZ,
    -- Stable dedup fingerprint (hash of normalized name + size class + group).
    release_hash    TEXT NOT NULL UNIQUE,
    -- Post-processing state: pending -> done/failed.
    pp_status       TEXT NOT NULL DEFAULT 'pending',
    nfo             TEXT,
    grabs           BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_releases_category ON releases (category_id);
CREATE INDEX idx_releases_posted ON releases (posted_at DESC);
CREATE INDEX idx_releases_pp_status ON releases (pp_status);
CREATE INDEX idx_releases_search_name ON releases (search_name);

-- Per-file detail within a release, recovered during post-processing
-- (e.g. from PAR2). Also stores the ordered segments needed to rebuild the
-- NZB for that file.
CREATE TABLE release_files (
    id              BIGSERIAL PRIMARY KEY,
    release_id      BIGINT NOT NULL REFERENCES releases (id) ON DELETE CASCADE,
    file_name       TEXT NOT NULL DEFAULT '',
    size_bytes      BIGINT NOT NULL DEFAULT 0,
    -- Ordered NZB segments as JSON: [{"message_id","bytes","number"}...].
    segments        JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_release_files_release ON release_files (release_id);

-- Local user accounts.
CREATE TABLE users (
    id              BIGSERIAL PRIMARY KEY,
    username        TEXT NOT NULL UNIQUE,
    -- bcrypt hash.
    password_hash   TEXT NOT NULL,
    -- 'admin' or 'user'.
    role            TEXT NOT NULL DEFAULT 'user',
    -- Per-user request budget per rate-limit window (0 = use default).
    rate_limit      INTEGER NOT NULL DEFAULT 0,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Newznab API keys, one or more per user.
CREATE TABLE api_keys (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    api_key         TEXT NOT NULL UNIQUE,
    label           TEXT NOT NULL DEFAULT '',
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    last_used_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_api_keys_user ON api_keys (user_id);
