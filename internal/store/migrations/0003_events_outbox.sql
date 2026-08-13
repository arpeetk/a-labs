-- 0003_events_outbox: durable intent log and immutable per-run event journal.
--
-- A run submission and its launch operation are committed in one transaction.
-- A worker may therefore die before or after publishing the AgentRun and a
-- second replica can safely replay the operation. source+source_id makes event
-- ingestion idempotent across gateway restarts and CR reconciliation loops.

CREATE TABLE run_events (
    id          bigserial   PRIMARY KEY,
    run_id      text        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    source      text        NOT NULL,
    source_id   text        NOT NULL,
    event_type  text        NOT NULL,
    payload     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL,
    UNIQUE (run_id, source, source_id)
);

CREATE INDEX run_events_run_order_idx ON run_events (run_id, id);

CREATE TABLE outbox_operations (
    id           text        PRIMARY KEY,
    run_id       text        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind         text        NOT NULL,
    payload      jsonb       NOT NULL,
    state        text        NOT NULL DEFAULT 'pending'
                 CHECK (state IN ('pending', 'processing', 'completed', 'failed')),
    attempts     integer     NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL,
    lease_owner  text        NOT NULL DEFAULT '',
    lease_until  timestamptz,
    last_error   text        NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL,
    updated_at   timestamptz NOT NULL
);

CREATE INDEX outbox_ready_idx
    ON outbox_operations (state, available_at, created_at)
    WHERE state IN ('pending', 'processing');
