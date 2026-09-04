-- Persistent pipeline jobs (#113): manual and scheduled pipeline operations are
-- recorded as observable jobs with a stable id, lifecycle state, progress,
-- cooperative cancellation, and retained history, so callers can see when work
-- started, whether it finished, why it failed, and cancel it — surviving
-- restarts (running jobs are marked interrupted on startup).
CREATE TABLE jobs (
    -- Client-stable identifier (UUID text) returned by trigger endpoints.
    id            TEXT PRIMARY KEY,
    -- Job type: scan, backfill, assemble, build, postprocess, enrich,
    -- retention, maintenance, ...
    type          TEXT NOT NULL,
    -- Lifecycle state: queued, running, completed, failed, cancelled,
    -- interrupted.
    state         TEXT NOT NULL DEFAULT 'queued',
    -- Optional target (e.g. a single group name); empty means all/none.
    target        TEXT NOT NULL DEFAULT '',
    -- Progress counters (0 total = indeterminate).
    progress_current BIGINT NOT NULL DEFAULT 0,
    progress_total   BIGINT NOT NULL DEFAULT 0,
    -- Human-readable current-activity message.
    message       TEXT NOT NULL DEFAULT '',
    -- Error message when state = failed.
    error         TEXT NOT NULL DEFAULT '',
    -- Set true to request cooperative cancellation; the worker polls it.
    cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ
);

-- History listing is newest-first.
CREATE INDEX idx_jobs_created ON jobs (created_at DESC);
-- Cheap "is anything active" / recovery scans on the small set of live jobs.
CREATE INDEX idx_jobs_active ON jobs (state) WHERE state IN ('queued', 'running');
