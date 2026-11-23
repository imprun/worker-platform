-- The v2 writer used an untagged WorkerProfile.Spec field in the snapshot.
INSERT INTO wp_job_revisions (worker_uid, revision, snapshot) VALUES (
    'legacy-worker-uid', 'v1', '{
        "profile": {"Spec": {"atespace": "legacy", "workerPool": {"namespace": "ate-system", "name": "legacy"}}},
        "template": {"containers": [{"image": "echo:v1"}]},
        "templateUID": "legacy-template-uid"
    }'
);

INSERT INTO wp_job_runs (id, namespace, worker, worker_uid, revision, idempotency_key, input, state, attempt_no, deadline_at, lease_id, lease_until)
VALUES
    ('00000000-0000-4000-8000-000000000001', 'legacy', 'echo', 'legacy-worker-uid', 'v1', 'active', '{}', 'RUNNING', 1,
        clock_timestamp()+interval '5 minutes', '00000000-0000-4000-8000-000000000003', clock_timestamp()+interval '2 minutes'),
    ('00000000-0000-4000-8000-000000000002', 'legacy', 'echo', 'legacy-worker-uid', 'v1', 'complete', '{}', 'SUCCEEDED', 1,
        clock_timestamp()+interval '5 minutes', NULL, NULL);

INSERT INTO wp_job_attempts (run_id, number, state, actor_space, actor_name, actor_uid, worker_id, worker_pod_uid)
VALUES
    ('00000000-0000-4000-8000-000000000001', 1, 'RUNNING', 'legacy', 'legacy-active', 'legacy-active-uid', 'worker', 'worker-pod'),
    ('00000000-0000-4000-8000-000000000002', 1, 'SUCCEEDED', 'legacy', 'legacy-complete', 'legacy-complete-uid', 'worker', 'worker-pod');

INSERT INTO wp_job_results (run_id, attempt_no, value) VALUES
    ('00000000-0000-4000-8000-000000000002', 1, '{"version":"v1"}');
