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

ALTER TABLE worker_apps ADD COLUMN IF NOT EXISTS deletion_request text NOT NULL DEFAULT '';
ALTER TABLE worker_apps ADD COLUMN IF NOT EXISTS deleted_at timestamptz;
CREATE UNIQUE INDEX IF NOT EXISTS worker_apps_active_name
    ON worker_apps (organization_id, project_id, name) WHERE deleted_at IS NULL;
ALTER TABLE worker_apps DROP CONSTRAINT IF EXISTS worker_apps_organization_id_project_id_name_key;
