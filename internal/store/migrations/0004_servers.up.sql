-- News (NNTP) servers configurable at runtime from the UI. The pool selects
-- the highest-priority enabled server. Designed to support multiple servers
-- (e.g. a primary plus a block/fill account) even though v1 uses one active
-- server at a time.
CREATE TABLE servers (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL,
    host            TEXT NOT NULL,
    port            INTEGER NOT NULL DEFAULT 563,
    tls             BOOLEAN NOT NULL DEFAULT TRUE,
    username        TEXT NOT NULL DEFAULT '',
    -- Provider password. Stored plaintext (required to authenticate to NNTP);
    -- never returned by the API.
    password        TEXT NOT NULL DEFAULT '',
    max_conns       INTEGER NOT NULL DEFAULT 10,
    -- Lower priority value = preferred. Ties broken by id.
    priority        INTEGER NOT NULL DEFAULT 0,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_servers_active ON servers (enabled, priority, id);
