-- 0001_init: the two control-plane tables (spec §5.2).
--
-- projects and runs mirror internal/store.Project and .Run. The egress
-- allowlist is a text[] (a scalar list, not a relation — matches the Go slice
-- one-for-one). created_at is stored as timestamptz; the store round-trips it
-- back to time.Time in UTC. A restarted apiserver re-learns in-flight runs by
-- upserting from the AgentRun CRs (reconcile-on-boot), so runs must be
-- upsertable by primary key.

CREATE TABLE projects (
    name              text PRIMARY KEY,
    repo              text        NOT NULL DEFAULT '',
    default_harness   text        NOT NULL DEFAULT '',
    harness_image     text        NOT NULL DEFAULT '',
    default_model     text        NOT NULL DEFAULT '',
    runtime_class     text        NOT NULL DEFAULT '',
    cpu               text        NOT NULL DEFAULT '',
    memory            text        NOT NULL DEFAULT '',
    disk              text        NOT NULL DEFAULT '',
    checkpoint_bucket text        NOT NULL DEFAULT '',
    egress_allowlist  text[]      NOT NULL DEFAULT '{}',
    namespace         text        NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL
);

CREATE TABLE runs (
    id            text PRIMARY KEY,
    project       text        NOT NULL DEFAULT '',
    "user"        text        NOT NULL DEFAULT '',
    prompt        text        NOT NULL DEFAULT '',
    harness       text        NOT NULL DEFAULT '',
    model         text        NOT NULL DEFAULT '',
    base_ref      text        NOT NULL DEFAULT '',
    interactive   boolean     NOT NULL DEFAULT false,
    runtime       text        NOT NULL DEFAULT '',
    namespace     text        NOT NULL DEFAULT '',
    phase         text        NOT NULL DEFAULT '',
    pr_url        text        NOT NULL DEFAULT '',
    restart_count integer     NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL
);

-- ListRuns filters by user (scope=mine) and project, ordered newest-first.
CREATE INDEX runs_user_idx    ON runs ("user");
CREATE INDEX runs_project_idx ON runs (project);
CREATE INDEX runs_created_idx ON runs (created_at DESC, id);
