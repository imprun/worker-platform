CREATE TABLE IF NOT EXISTS wp_job_revisions (
    worker_uid text NOT NULL,
    revision text NOT NULL,
    snapshot jsonb NOT NULL,
    PRIMARY KEY (worker_uid, revision)
);

CREATE TABLE IF NOT EXISTS wp_job_runs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace text NOT NULL,
    worker text NOT NULL,
    worker_uid text NOT NULL,
    revision text NOT NULL,
    idempotency_key text NOT NULL,
    input jsonb NOT NULL,
    state text NOT NULL DEFAULT 'QUEUED' CHECK (state IN ('QUEUED', 'STARTING', 'RUNNING', 'RELEASING', 'SUCCEEDED', 'FAILED')),
    attempt_no integer NOT NULL DEFAULT 0,
    reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    due_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    deadline_at timestamptz NOT NULL,
    lease_id uuid,
    lease_until timestamptz,
    UNIQUE (worker_uid, idempotency_key),
    FOREIGN KEY (worker_uid, revision) REFERENCES wp_job_revisions (worker_uid, revision),
    CHECK ((lease_id IS NULL) = (lease_until IS NULL))
);

CREATE INDEX IF NOT EXISTS wp_job_dispatch ON wp_job_runs (namespace, due_at, created_at)
    WHERE state IN ('QUEUED', 'STARTING', 'RUNNING', 'RELEASING');

CREATE TABLE IF NOT EXISTS wp_job_attempts (
    run_id uuid NOT NULL REFERENCES wp_job_runs (id),
    number integer NOT NULL CHECK (number > 0),
    state text NOT NULL CHECK (state IN ('STARTING', 'RUNNING', 'RELEASING', 'SUCCEEDED', 'FAILED')),
    actor_space text NOT NULL,
    actor_name text NOT NULL,
    actor_uid text NOT NULL DEFAULT '',
    worker_id text NOT NULL DEFAULT '',
    worker_pod_uid text NOT NULL DEFAULT '',
    reason text NOT NULL DEFAULT '',
    retryable boolean NOT NULL DEFAULT false,
    outcome jsonb,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz,
    PRIMARY KEY (run_id, number),
    UNIQUE (actor_space, actor_name)
);

CREATE TABLE IF NOT EXISTS wp_job_results (
    run_id uuid PRIMARY KEY,
    attempt_no integer NOT NULL,
    value jsonb NOT NULL,
    FOREIGN KEY (run_id, attempt_no) REFERENCES wp_job_attempts (run_id, number)
);
