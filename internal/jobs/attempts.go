package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	ate "go.ate.dev/substrate/api/ateapi/v1"
)

func (s *Store) startAttempt(ctx context.Context, job claim) (Attempt, error) {
	number := job.number + 1
	name := fmt.Sprintf("wp-job-%s-%d", strings.ReplaceAll(job.id, "-", ""), number)
	var attempt Attempt
	err := s.fenced(ctx, job, func(tx pgx.Tx) error {
		if err := lockPoolSlot(ctx, tx, job.workerPool); err != nil {
			return err
		}
		changed, err := tx.Exec(ctx, `
			UPDATE wp_job_runs SET state='STARTING', attempt_no=$2, reason=''
			WHERE id=$1 AND state='QUEUED' AND attempt_no=$2-1
		`, job.id, number)
		if err := requireRow(changed, err); err != nil {
			return err
		}
		return scanAttempt(tx.QueryRow(ctx, `
			INSERT INTO wp_job_attempts (run_id, number, state, actor_space, actor_name)
			VALUES ($1, $2, 'STARTING', $3, $4) RETURNING `+attemptFields,
			job.id, number, job.revision.Profile.Spec.Atespace, name), &attempt)
	})
	return attempt, err
}

func (s *Store) recordActor(ctx context.Context, job claim, uid string) error {
	if uid == "" {
		return errors.New("empty Actor UID")
	}
	return s.fenced(ctx, job, func(tx pgx.Tx) error {
		changed, err := tx.Exec(ctx, `
			UPDATE wp_job_attempts SET actor_uid=$3
			WHERE run_id=$1 AND number=$2 AND state='STARTING' AND actor_uid IN ('', $3)
		`, job.id, job.number, uid)
		return requireRow(changed, err)
	})
}

func (s *Store) startInvocation(ctx context.Context, job claim, uid string, assignment *ate.WorkerAssignment, sessionVersion int64) error {
	return s.fenced(ctx, job, func(tx pgx.Tx) error {
		changed, err := tx.Exec(ctx, `
			UPDATE wp_job_attempts SET state='RUNNING', worker_id=$4, worker_pod_uid=$5, site_session_version=$6
			WHERE run_id=$1 AND number=$2 AND state='STARTING' AND actor_uid=$3 AND actor_uid<>''
		`, job.id, job.number, uid, assignment.GetWorker().GetName(), assignment.GetWorkerPodUid(), sessionVersion)
		if err := requireRow(changed, err); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE wp_job_runs SET state='RUNNING' WHERE id=$1`, job.id)
		return err
	})
}

func (s *Store) recordOutcome(ctx context.Context, job claim, value outcome) error {
	save := func(value outcome) error {
		return s.fenced(ctx, job, func(tx pgx.Tx) error {
			changed, err := tx.Exec(ctx, `
				UPDATE wp_job_attempts SET state='RELEASING', outcome=$3::jsonb, reason=$4, retryable=$5,
					reauth_required=$6::bigint>0, site_session_version=GREATEST(site_session_version,$6)
				WHERE run_id=$1 AND number=$2 AND state IN ('STARTING','RUNNING')
			`, job.id, job.number, value.result, value.reason, value.retryable, value.reauthVersion)
			if err := requireRow(changed, err); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE wp_job_runs SET state='RELEASING', reason=$2 WHERE id=$1`, job.id, value.reason)
			return err
		})
	}
	if err := save(value); invalidJSON(err) {
		return save(outcome{reason: "invalid_result"})
	} else {
		return err
	}
}

func (s *Store) finishAttempt(ctx context.Context, job claim) error {
	return s.fenced(ctx, job, func(tx pgx.Tx) error {
		var hasResult, canRetry, needsAuth, inTime bool
		var reason string
		err := tx.QueryRow(ctx, `
			SELECT a.outcome IS NOT NULL, a.retryable AND 1+(SELECT count(*) FROM wp_job_attempts previous
				WHERE previous.run_id=r.id AND previous.state='FAILED' AND NOT previous.reauth_required)<$3,
				a.reason, a.reauth_required, r.deadline_at>clock_timestamp()
			FROM wp_job_attempts a JOIN wp_job_runs r ON r.id=a.run_id
			WHERE a.run_id=$1 AND a.number=$2 AND a.state='RELEASING'
		`, job.id, job.number, maxAttempts).Scan(&hasResult, &canRetry, &reason, &needsAuth, &inTime)
		if err != nil {
			return err
		}
		attemptState, runState := failed, failed
		switch {
		case hasResult:
			attemptState, runState = succeeded, succeeded
			if _, err := tx.Exec(ctx, `
				INSERT INTO wp_job_results (run_id, attempt_no, value)
				SELECT run_id, number, outcome FROM wp_job_attempts WHERE run_id=$1 AND number=$2
			`, job.id, job.number); err != nil {
				return err
			}
		case needsAuth && inTime:
			runState = waitingAuth
			if _, err := tx.Exec(ctx, `
				INSERT INTO wp_job_reauth_requests (run_id, attempt_no, site_session_id, expected_version, expires_at)
				SELECT r.id, a.number, r.site_session_id, a.site_session_version, r.deadline_at
				FROM wp_job_runs r JOIN wp_job_attempts a ON a.run_id=r.id AND a.number=r.attempt_no
				WHERE r.id=$1
			`, job.id); err != nil {
				return err
			}
		case needsAuth:
			reason = "reauth_deadline"
		case canRetry && inTime:
			runState = queued
		}
		if _, err := tx.Exec(ctx, `
			UPDATE wp_job_attempts SET state=$3, finished_at=clock_timestamp(), outcome=NULL
			WHERE run_id=$1 AND number=$2
		`, job.id, job.number, attemptState); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE wp_job_runs SET state=$2, reason=$3, lease_id=NULL, lease_until=NULL, due_at=clock_timestamp()+interval '1 second'
			WHERE id=$1
		`, job.id, runState, reason)
		return err
	})
}

func (s *Store) expireQueued(ctx context.Context, job claim) error {
	changed, err := s.pool.Exec(ctx, `
		UPDATE wp_job_runs SET state='FAILED', reason='run_deadline', lease_id=NULL, lease_until=NULL
		WHERE id=$1 AND lease_id=$2 AND state='QUEUED' AND lease_until>clock_timestamp()
	`, job.id, job.leaseID)
	return requireRow(changed, err)
}
