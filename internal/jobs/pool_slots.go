package jobs

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

var errPoolFull = errors.New("WorkerPool has no free Job slots")

// lockPoolSlot serializes reservation and limit edits for one physical pool.
// The caller records the new Attempt in the same transaction.
func lockPoolSlot(ctx context.Context, tx pgx.Tx, pool string) error {
	if pool == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wp_job_pool_limits (worker_pool) VALUES ($1) ON CONFLICT DO NOTHING`, pool); err != nil {
		return err
	}
	var limit *int32
	if err := tx.QueryRow(ctx, `SELECT max_slots FROM wp_job_pool_limits WHERE worker_pool=$1 FOR UPDATE`, pool).Scan(&limit); err != nil {
		return err
	}
	if limit == nil {
		return nil
	}
	// Read usage in a separate statement after acquiring the row lock so a
	// waiting replica sees reservations committed by the previous holder.
	var available bool
	err := tx.QueryRow(ctx, `SELECT count(*) < $2 FROM wp_job_runs r
		JOIN wp_job_revisions v USING (worker_uid, revision)
		WHERE v.worker_pool=$1 AND r.state IN ('STARTING', 'RUNNING', 'RELEASING')`, pool, *limit).Scan(&available)
	if err == nil && !available {
		return errPoolFull
	}
	return err
}
