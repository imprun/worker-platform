CREATE TABLE wp_job_pool_limits (
    worker_pool text PRIMARY KEY,
    max_slots integer CHECK (max_slots >= 0)
);

-- Existing Job snapshots encode the WorkerProfile.Spec field as "Spec".
ALTER TABLE wp_job_revisions ADD COLUMN worker_pool text GENERATED ALWAYS AS (
    NULLIF(snapshot #>> '{profile,Spec,workerPool,namespace}', '') || '/' ||
    NULLIF(snapshot #>> '{profile,Spec,workerPool,name}', '')
) STORED;

CREATE INDEX wp_job_revisions_by_pool ON wp_job_revisions (worker_pool, worker_uid, revision);
CREATE INDEX wp_job_occupied_runs_by_revision ON wp_job_runs (worker_uid, revision)
    WHERE state IN ('STARTING', 'RUNNING', 'RELEASING');
