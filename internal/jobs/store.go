package jobs

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/imprun/worker-platform/internal/platform"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

//go:embed schema_site_sessions.sql
var siteSessionSchema string

//go:embed schema_pool_slots.sql
var poolSlotSchema string

//go:embed schema_publications.sql
var publicationSchema string

type Store struct {
	pool       *pgxpool.Pool
	namespace  string
	siteCipher cipher.AEAD
}

func OpenFiles(ctx context.Context, namespace, dsnFile, siteKeyFile string) (*Store, error) {
	dsn, err := os.ReadFile(dsnFile)
	if err != nil || strings.TrimSpace(string(dsn)) == "" {
		return nil, errors.New("read nonempty Job PostgreSQL DSN file")
	}
	var siteKey []byte
	if siteKeyFile != "" {
		encoded, err := os.ReadFile(siteKeyFile)
		if err != nil {
			return nil, errors.New("read site session key file")
		}
		siteKey, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
		if err != nil || len(siteKey) != 32 {
			return nil, errors.New("site session key must encode 32 bytes in base64")
		}
	}
	return Open(ctx, strings.TrimSpace(string(dsn)), namespace, siteKey)
}

func Open(ctx context.Context, dsn, namespace string, siteKey []byte) (*Store, error) {
	var siteCipher cipher.AEAD
	if len(siteKey) != 0 {
		if len(siteKey) != 32 {
			return nil, errors.New("site session encryption requires a 32-byte key")
		}
		block, err := aes.NewCipher(siteKey)
		if err != nil {
			return nil, err
		}
		siteCipher, err = cipher.NewGCMWithRandomNonce(block)
		if err != nil {
			return nil, err
		}
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL DSN")
	}
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, errors.New("configure PostgreSQL pool")
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1230002533, 1)"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, schemaSQL); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS wp_job_migrations (version integer PRIMARY KEY)"); err != nil {
			return err
		}
		for _, migration := range []struct {
			version int
			sql     string
		}{{2, siteSessionSchema}, {3, poolSlotSchema}, {4, publicationSchema}} {
			added, err := tx.Exec(ctx, "INSERT INTO wp_job_migrations VALUES ($1) ON CONFLICT DO NOTHING", migration.version)
			if err != nil {
				return err
			}
			if added.RowsAffected() == 1 {
				if _, err := tx.Exec(ctx, migration.sql); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		pool.Close()
		return nil, errors.New("initialize PostgreSQL Job schema")
	}
	return &Store{pool: pool, namespace: namespace, siteCipher: siteCipher}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func recordRevision(ctx context.Context, tx pgx.Tx, workerUID, revision string, snapshot platform.JobRevision) error {
	cfg, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
			INSERT INTO wp_job_revisions (worker_uid, revision, snapshot) VALUES ($1, $2, $3::jsonb)
			ON CONFLICT (worker_uid, revision) DO NOTHING
	`, workerUID, revision, cfg); err != nil {
		return err
	}
	var uid string
	err = tx.QueryRow(ctx, `
			SELECT snapshot->>'templateUID' FROM wp_job_revisions WHERE worker_uid=$1 AND revision=$2
	`, workerUID, revision).Scan(&uid)
	if err != nil {
		return err
	}
	if uid != snapshot.TemplateUID {
		return errors.New("published Job revision has a different ActorTemplate UID")
	}
	return nil
}

func scanRun(row pgx.Row, result *Run, extra ...any) error {
	targets := []any{&result.ID, &result.Worker, &result.Revision, &result.State, &result.Reason, &result.CreatedAt, &result.DeadlineAt}
	return row.Scan(append(targets, extra...)...)
}

const runFields = "id::text, worker, revision, state, reason, created_at, deadline_at"
const attemptFields = "number, state, actor_space, actor_name, actor_uid, worker_id, worker_pod_uid, reason, created_at, site_session_version"

func scanAttempt(row pgx.Row, attempt *Attempt) error {
	return row.Scan(&attempt.Number, &attempt.State, &attempt.ActorSpace, &attempt.Actor, &attempt.ActorUID,
		&attempt.WorkerID, &attempt.WorkerPodUID, &attempt.Reason, &attempt.CreatedAt, &attempt.SiteSessionVersion)
}

func (s *Store) Admit(ctx context.Context, worker, key string, input json.RawMessage, sessionRef string) (Run, error) {
	var run Run
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var workerUID, revision string
		err := tx.QueryRow(ctx, `SELECT worker_uid, COALESCE(revision, '') FROM wp_job_publications
			WHERE namespace=$1 AND worker=$2 AND enabled FOR SHARE`, s.namespace, worker).
			Scan(&workerUID, &revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		replay := func() (bool, error) {
			var same bool
			err := scanRun(tx.QueryRow(ctx, `
				SELECT `+runFields+`, input=$4::jsonb AND
					((SELECT ref FROM wp_job_site_sessions WHERE id=r.site_session_id) IS NOT DISTINCT FROM NULLIF($5,''))
				FROM wp_job_runs r
				WHERE namespace=$1 AND worker_uid=$2 AND idempotency_key=$3
			`, s.namespace, workerUID, key, input, sessionRef), &run, &same)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err == nil && !same {
				err = ErrInputConflict
			}
			return err == nil, err
		}
		if found, err := replay(); found || err != nil {
			return err
		}
		if revision == "" {
			return ErrNotPublished
		}
		var siteSessionID string
		if sessionRef != "" {
			if s.siteCipher == nil {
				return ErrAuthDisabled
			}
			err := tx.QueryRow(ctx, `SELECT id::text FROM wp_job_site_sessions WHERE namespace=$1 AND worker_uid=$2 AND ref=$3`,
				s.namespace, workerUID, sessionRef).Scan(&siteSessionID)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
		}
		err = scanRun(tx.QueryRow(ctx, `
			INSERT INTO wp_job_runs (namespace, worker, worker_uid, revision, idempotency_key, input, deadline_at, site_session_id)
			SELECT $1, $2, worker_uid, revision, $5, $6::jsonb, clock_timestamp()+make_interval(secs => $7), NULLIF($8,'')::uuid
			FROM wp_job_revisions WHERE worker_uid=$3 AND revision=$4
			ON CONFLICT (worker_uid, idempotency_key) DO NOTHING
			RETURNING `+runFields, s.namespace, worker, workerUID, revision, key, input, runLifetime.Seconds(), siteSessionID), &run)
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if found, err := replay(); found || err != nil {
			return err
		}
		return ErrNotPublished
	})
	if invalidJSON(err) {
		err = ErrInvalidJSON
	}
	return run, err
}

func (s *Store) Get(ctx context.Context, id string) (Run, error) {
	run := Run{Attempts: []Attempt{}}
	options := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	err := pgx.BeginTxFunc(ctx, s.pool, options, func(tx pgx.Tx) error {
		err := scanRun(tx.QueryRow(ctx, "SELECT "+runFields+" FROM wp_job_runs WHERE namespace=$1 AND id=$2", s.namespace, id), &run)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT "+attemptFields+" FROM wp_job_attempts WHERE run_id=$1 ORDER BY number", id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var attempt Attempt
			if err := scanAttempt(rows, &attempt); err != nil {
				return err
			}
			run.Attempts = append(run.Attempts, attempt)
			switch attempt.State {
			case starting, running, releasing:
				run.OccupiedSlots++
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if run.State == waitingAuth {
			run.Reauth = new(ReauthRequest)
			err := scanReauth(tx.QueryRow(ctx, `SELECT `+reauthFields+` FROM wp_job_reauth_requests
				WHERE run_id=$1 AND attempt_no=(SELECT attempt_no FROM wp_job_runs WHERE id=$1)
					AND state IN ('PENDING','STARTED')`, id), run.Reauth)
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("waiting Run is missing its active reauthentication request")
			}
			return err
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return run, err
}

func (s *Store) Result(ctx context.Context, id string) (string, json.RawMessage, error) {
	var state string
	var result json.RawMessage
	err := s.pool.QueryRow(ctx, `
		SELECT r.state, v.value FROM wp_job_runs r LEFT JOIN wp_job_results v ON v.run_id=r.id
		WHERE r.namespace=$1 AND r.id=$2
	`, s.namespace, id).Scan(&state, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err == nil && state == succeeded && result == nil {
		return "", nil, errors.New("successful Run is missing its result")
	}
	return state, result, err
}

func (s *Store) claim(ctx context.Context, busyPools ...string) (claim, error) {
	var job claim
	var snapshot []byte
	err := s.pool.QueryRow(ctx, `
		WITH occupied AS (
			SELECT v.worker_pool, count(*) AS slots FROM wp_job_runs r
			JOIN wp_job_revisions v USING (worker_uid, revision)
			WHERE r.state IN ('STARTING', 'RUNNING', 'RELEASING') GROUP BY v.worker_pool
		), candidate AS (
			SELECT r.id FROM wp_job_runs r
			JOIN wp_job_revisions v USING (worker_uid, revision)
			LEFT JOIN wp_job_pool_limits p USING (worker_pool)
			LEFT JOIN occupied o USING (worker_pool)
			WHERE r.namespace=$1
				AND COALESCE(v.worker_pool,'') <> ALL(COALESCE($3::text[], '{}'))
				AND (r.state IN ('QUEUED', 'STARTING', 'RUNNING', 'RELEASING') OR (r.state='WAITING_AUTH' AND r.deadline_at<=clock_timestamp()))
				AND r.due_at<=clock_timestamp() AND (r.lease_until IS NULL OR r.lease_until<=clock_timestamp())
				AND (r.state<>'QUEUED' OR r.deadline_at<=clock_timestamp() OR p.max_slots IS NULL OR COALESCE(o.slots,0)<p.max_slots)
			ORDER BY r.due_at, r.created_at, r.id FOR UPDATE OF r SKIP LOCKED LIMIT 1
		), claimed AS (
			UPDATE wp_job_runs r SET lease_id=gen_random_uuid(), lease_until=clock_timestamp()+make_interval(secs => $2)
			FROM candidate c WHERE r.id=c.id RETURNING r.*
		)
		SELECT c.id::text, c.lease_id::text, c.state, c.attempt_no, c.deadline_at, c.input, COALESCE(c.site_session_id::text,''),
			COALESCE(v.worker_pool,''), v.snapshot
		FROM claimed c JOIN wp_job_revisions v USING (worker_uid, revision)
	`, s.namespace, leaseLifetime.Seconds(), busyPools).Scan(&job.id, &job.leaseID, &job.state, &job.number, &job.deadline, &job.input,
		&job.siteSessionID, &job.workerPool, &snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return claim{}, nil
	}
	if err != nil {
		return claim{}, err
	}
	return job, json.Unmarshal(snapshot, &job.revision)
}

func (s *Store) renew(ctx context.Context, job claim) error {
	changed, err := s.pool.Exec(ctx, `
		UPDATE wp_job_runs SET lease_until=clock_timestamp()+make_interval(secs => $3)
		WHERE id=$1 AND lease_id=$2 AND lease_until>clock_timestamp()
	`, job.id, job.leaseID, leaseLifetime.Seconds())
	return requireRow(changed, err)
}

func (s *Store) yield(ctx context.Context, job claim, delay time.Duration) error {
	changed, err := s.pool.Exec(ctx, `
		UPDATE wp_job_runs SET lease_id=NULL, lease_until=NULL, due_at=clock_timestamp()+make_interval(secs => $3)
		WHERE id=$1 AND lease_id=$2 AND lease_until>clock_timestamp()
	`, job.id, job.leaseID, delay.Seconds())
	return requireRow(changed, err)
}

func (s *Store) fenced(ctx context.Context, job claim, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `SELECT id::text FROM wp_job_runs
			WHERE id=$1 AND lease_id=$2 AND lease_until>clock_timestamp() FOR UPDATE`, job.id, job.leaseID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errLeaseLost
		}
		if err != nil {
			return err
		}
		return fn(tx)
	})
}

func requireRow(changed pgconn.CommandTag, err error) error {
	if err == nil && changed.RowsAffected() != 1 {
		return errLeaseLost
	}
	return err
}

func invalidJSON(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "22P02", "22P05", "22003", "22021":
		return true
	default:
		return false
	}
}

func (s *Store) activeAttempt(ctx context.Context, job claim) (Attempt, error) {
	var attempt Attempt
	err := scanAttempt(s.pool.QueryRow(ctx, "SELECT "+attemptFields+" FROM wp_job_attempts WHERE run_id=$1 AND number=$2", job.id, job.number), &attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = fmt.Errorf("Run is missing Attempt %d", job.number)
	}
	return attempt, err
}
