package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

type SiteStateUpdate struct {
	State     json.RawMessage `json:"state"`
	ExpiresAt time.Time       `json:"expiresAt"`
}

type siteSession struct {
	Ref     string          `json:"ref"`
	Version int64           `json:"version"`
	State   json.RawMessage `json:"state"`
}

type SiteSessionReceipt struct {
	Ref       string    `json:"ref"`
	Version   int64     `json:"version"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func sessionAAD(id string, version int64) []byte {
	return []byte(id + "/" + strconv.FormatInt(version, 10))
}

func (s *Store) CreateSiteSession(ctx context.Context, workerUID, ref string, update SiteStateUpdate) (SiteSessionReceipt, error) {
	if s.siteCipher == nil {
		return SiteSessionReceipt{}, ErrAuthDisabled
	}
	var id string
	if err := s.pool.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		return SiteSessionReceipt{}, err
	}
	sealed := s.siteCipher.Seal(nil, nil, update.State, sessionAAD(id, 1))
	var future bool
	var version int64
	err := s.pool.QueryRow(ctx, `
		WITH validity AS (SELECT $6::timestamptz>clock_timestamp() AS future), created AS (
			INSERT INTO wp_job_site_sessions (id, namespace, worker_uid, ref, version, encrypted_state, expires_at)
			SELECT $1, $2, $3, $4, 1, $5, $6 FROM validity WHERE future
			ON CONFLICT (namespace, worker_uid, ref) DO NOTHING
			RETURNING version
		) SELECT future, COALESCE((SELECT version FROM created),0) FROM validity
	`, id, s.namespace, workerUID, ref, sealed, update.ExpiresAt).Scan(&future, &version)
	if err != nil {
		return SiteSessionReceipt{}, err
	}
	if !future {
		return SiteSessionReceipt{}, ErrInvalidExpiry
	}
	if version == 0 {
		return SiteSessionReceipt{}, ErrAuthConflict
	}
	return SiteSessionReceipt{Ref: ref, Version: version, ExpiresAt: update.ExpiresAt}, nil
}

func (s *Store) sessionForAttempt(ctx context.Context, id string) (*siteSession, bool, error) {
	if id == "" {
		return nil, true, nil
	}
	if s.siteCipher == nil {
		return nil, false, ErrAuthDisabled
	}
	var result siteSession
	var encrypted []byte
	var valid bool
	err := s.pool.QueryRow(ctx, `
		SELECT ref, version, encrypted_state, expires_at>clock_timestamp()
		FROM wp_job_site_sessions WHERE id=$1 AND namespace=$2
	`, id, s.namespace).Scan(&result.Ref, &result.Version, &encrypted, &valid)
	if err != nil {
		return nil, false, err
	}
	if !valid {
		return &result, false, nil
	}
	plaintext, err := s.siteCipher.Open(nil, nil, encrypted, sessionAAD(id, result.Version))
	if err != nil {
		return nil, false, errors.New("cannot decrypt site session")
	}
	result.State = plaintext
	return &result, true, nil
}

type ReauthRequest struct {
	ID             string    `json:"requestId"`
	State          string    `json:"state"`
	ExpiresAt      time.Time `json:"expiresAt"`
	SessionVersion int64     `json:"siteSessionVersion,omitempty"`
}

const (
	authPending   = "PENDING"
	authStarted   = "STARTED"
	authCompleted = "COMPLETED"
	reauthFields  = "id::text, state, expires_at, COALESCE(completed_version,0)"
)

func scanReauth(row pgx.Row, r *ReauthRequest) error {
	return row.Scan(&r.ID, &r.State, &r.ExpiresAt, &r.SessionVersion)
}

func (s *Store) StartReauth(ctx context.Context, runID, id string) (ReauthRequest, error) {
	return s.advanceReauth(ctx, runID, id, nil)
}

func (s *Store) CompleteReauth(ctx context.Context, runID, id string, update SiteStateUpdate) (ReauthRequest, error) {
	return s.advanceReauth(ctx, runID, id, &update)
}

func (s *Store) advanceReauth(ctx context.Context, runID, id string, update *SiteStateUpdate) (ReauthRequest, error) {
	var request ReauthRequest
	if s.siteCipher == nil {
		return request, ErrAuthDisabled
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var runState string
		var active int
		err := tx.QueryRow(ctx, `SELECT state, attempt_no FROM wp_job_runs
			WHERE id=$1 AND namespace=$2 FOR UPDATE`, runID, s.namespace).Scan(&runState, &active)
		if err != nil {
			return err
		}
		var siteID string
		var expected int64
		var attempt int
		var inTime bool
		err = tx.QueryRow(ctx, `
			SELECT `+reauthFields+`, site_session_id::text, expected_version, attempt_no, expires_at>clock_timestamp()
			FROM wp_job_reauth_requests WHERE id=$1 AND run_id=$2
		`, id, runID).Scan(&request.ID, &request.State, &request.ExpiresAt, &request.SessionVersion,
			&siteID, &expected, &attempt, &inTime)
		if err != nil || request.State == authCompleted {
			return err
		}
		if runState != waitingAuth || active != attempt || !inTime ||
			(request.State != authPending && request.State != authStarted) {
			return ErrAuthConflict
		}
		if update != nil && request.State != authStarted {
			return ErrAuthConflict
		}
		if update == nil && request.State == authStarted {
			return nil
		}
		var current int64
		var fresh bool
		err = tx.QueryRow(ctx, `SELECT version, expires_at>clock_timestamp() FROM wp_job_site_sessions
			WHERE id=$1 AND namespace=$2 FOR UPDATE`, siteID, s.namespace).Scan(&current, &fresh)
		if err != nil {
			return err
		}
		if current > expected && fresh {
			return completeRequest(ctx, tx, runID, &request, current)
		}
		if current < expected || (update != nil && current != expected) {
			return ErrAuthConflict
		}
		if update == nil {
			var busy bool
			err := tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM wp_job_reauth_requests WHERE site_session_id=$1
					AND state='STARTED' AND expires_at>clock_timestamp()
			)`, siteID).Scan(&busy)
			if err != nil {
				return err
			}
			if busy {
				return ErrAuthConflict
			}
			return scanReauth(tx.QueryRow(ctx, `
				UPDATE wp_job_reauth_requests SET state='STARTED', expected_version=$2
				WHERE id=$1 RETURNING `+reauthFields, id, current), &request)
		}
		version := current + 1
		sealed := s.siteCipher.Seal(nil, nil, update.State, sessionAAD(siteID, version))
		changed, err := tx.Exec(ctx, `UPDATE wp_job_site_sessions SET version=$2, encrypted_state=$3, expires_at=$4
			WHERE id=$1 AND $4::timestamptz>clock_timestamp()`, siteID, version, sealed, update.ExpiresAt)
		if err != nil {
			return err
		}
		if changed.RowsAffected() == 0 {
			return ErrInvalidExpiry
		}
		return completeRequest(ctx, tx, runID, &request, version)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ReauthRequest{}, ErrNotFound
	}
	return request, err
}

func completeRequest(ctx context.Context, tx pgx.Tx, runID string, request *ReauthRequest, version int64) error {
	err := scanReauth(tx.QueryRow(ctx, `UPDATE wp_job_reauth_requests SET state='COMPLETED', completed_version=$2
		WHERE id=$1 AND expires_at>clock_timestamp() RETURNING `+reauthFields, request.ID, version), request)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAuthConflict
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE wp_job_runs SET state='QUEUED', reason='', due_at=clock_timestamp(), lease_id=NULL, lease_until=NULL
		WHERE id=$1`, runID)
	return err
}

func (s *Store) expireReauth(ctx context.Context, job claim) error {
	return s.fenced(ctx, job, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE wp_job_reauth_requests SET state='EXPIRED'
			WHERE run_id=$1 AND state IN ('PENDING','STARTED')`, job.id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE wp_job_runs SET state='FAILED', reason='reauth_deadline', lease_id=NULL, lease_until=NULL
			WHERE id=$1 AND state='WAITING_AUTH'`, job.id)
		return err
	})
}
