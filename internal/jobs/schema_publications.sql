CREATE SEQUENCE wp_job_publication_epoch;

CREATE TABLE wp_job_publications (
    namespace text NOT NULL,
    worker text NOT NULL,
    worker_uid text NOT NULL,
    enabled boolean NOT NULL,
    epoch bigint NOT NULL,
    revision text,
    PRIMARY KEY (namespace, worker),
    FOREIGN KEY (worker_uid, revision) REFERENCES wp_job_revisions (worker_uid, revision)
);
