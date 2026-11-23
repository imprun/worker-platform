package jobs

import (
	"context"
	"errors"

	"github.com/imprun/worker-platform/internal/platform"
	"github.com/jackc/pgx/v5"
)

// PublicationEpoch orders observations of a worker across controller replicas,
// including deletion and replacement of a worker with the same name.
func (s *Store) PublicationEpoch(ctx context.Context) (int64, error) {
	var epoch int64
	err := s.pool.QueryRow(ctx, "SELECT nextval('wp_job_publication_epoch')").Scan(&epoch)
	return epoch, err
}

func (s *Store) PublishedWorkers(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, "SELECT worker FROM wp_job_publications WHERE namespace=$1 AND enabled", s.namespace)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *Store) ObserveWorker(ctx context.Context, worker, uid, priorRevision string, enabled bool, epoch int64) (bool, error) {
	// Only seed a missing publication from Kubernetes status when that revision
	// was already saved by the previous controller. Preparing a new generation
	// of the same worker preserves the last published revision.
	changed, err := s.pool.Exec(ctx, `
		INSERT INTO wp_job_publications AS p (namespace, worker, worker_uid, enabled, epoch, revision)
		VALUES ($1, $2, $3, $4, $5, (
			SELECT revision FROM wp_job_revisions WHERE worker_uid=$3 AND revision=$6 AND $4
		))
		ON CONFLICT (namespace, worker) DO UPDATE SET
			worker_uid=EXCLUDED.worker_uid, enabled=EXCLUDED.enabled, epoch=EXCLUDED.epoch,
			revision=CASE WHEN p.worker_uid=EXCLUDED.worker_uid AND p.enabled AND EXCLUDED.enabled
				THEN p.revision END
		WHERE p.epoch < EXCLUDED.epoch
	`, s.namespace, worker, uid, enabled, epoch, priorRevision)
	return changed.RowsAffected() == 1, err
}

func (s *Store) PublishRevision(ctx context.Context, worker, uid, revision string, epoch int64, snapshot platform.JobRevision) (bool, error) {
	var published bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var found int64
		err := tx.QueryRow(ctx, `SELECT epoch FROM wp_job_publications
			WHERE namespace=$1 AND worker=$2 AND worker_uid=$3 AND enabled AND epoch=$4 FOR UPDATE`,
			s.namespace, worker, uid, epoch).Scan(&found)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := recordRevision(ctx, tx, uid, revision, snapshot); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE wp_job_publications SET revision=$3 WHERE namespace=$1 AND worker=$2",
			s.namespace, worker, revision)
		published = err == nil
		return err
	})
	return published && err == nil, err
}

func (s *Store) publishedWorkerUID(ctx context.Context, worker string) (string, error) {
	var uid string
	err := s.pool.QueryRow(ctx, "SELECT worker_uid FROM wp_job_publications WHERE namespace=$1 AND worker=$2 AND enabled",
		s.namespace, worker).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return uid, err
}
