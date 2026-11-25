CREATE TABLE IF NOT EXISTS worker_apps (
    organization_id text NOT NULL,
    project_id text NOT NULL,
    app_id text NOT NULL,
    name text NOT NULL,
    profile text NOT NULL,
    package jsonb NOT NULL,
    PRIMARY KEY (organization_id, project_id, app_id),
    UNIQUE (organization_id, project_id, name)
);
