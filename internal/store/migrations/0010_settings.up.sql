-- Generic key/value settings table for runtime-configurable options (e.g. the
-- pipeline schedule editable from the admin UI). Values are stored as text and
-- parsed by the application, keeping the schema stable as new settings are
-- added.
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
