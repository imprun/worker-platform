CREATE TABLE wp_job_site_sessions (
    id uuid PRIMARY KEY,
    namespace text NOT NULL,
    worker_uid text NOT NULL,
    ref text NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    encrypted_state bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    UNIQUE (namespace, worker_uid, ref)
);

ALTER TABLE wp_job_runs ADD COLUMN site_session_id uuid REFERENCES wp_job_site_sessions (id);
ALTER TABLE wp_job_runs DROP CONSTRAINT wp_job_runs_state_check;
ALTER TABLE wp_job_runs ADD CONSTRAINT wp_job_runs_state_check
    CHECK (state IN ('QUEUED', 'STARTING', 'RUNNING', 'RELEASING', 'WAITING_AUTH', 'SUCCEEDED', 'FAILED'));
CREATE INDEX wp_job_auth_deadline ON wp_job_runs (namespace, deadline_at) WHERE state='WAITING_AUTH';

ALTER TABLE wp_job_attempts ADD COLUMN site_session_version bigint NOT NULL DEFAULT 0;
ALTER TABLE wp_job_attempts ADD COLUMN reauth_required boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX wp_job_one_occupied_slot ON wp_job_attempts (run_id)
    WHERE state IN ('STARTING', 'RUNNING', 'RELEASING');

CREATE TABLE wp_job_reauth_requests (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id uuid NOT NULL,
    attempt_no integer NOT NULL,
    site_session_id uuid NOT NULL REFERENCES wp_job_site_sessions (id),
    expected_version bigint NOT NULL CHECK (expected_version > 0),
    completed_version bigint,
    state text NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING', 'STARTED', 'COMPLETED', 'EXPIRED')),
    expires_at timestamptz NOT NULL,
    UNIQUE (run_id, attempt_no),
    FOREIGN KEY (run_id, attempt_no) REFERENCES wp_job_attempts (run_id, number)
);

CREATE INDEX wp_job_reauth_active_session ON wp_job_reauth_requests (site_session_id, expires_at)
    WHERE state='STARTED';
