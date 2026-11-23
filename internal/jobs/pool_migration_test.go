package jobs

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed testdata/pool-migration-v2.sql
var legacyPoolRows string

func TestPostgresPoolMigrationPreservesExistingRuns(t *testing.T) {
	dsn := os.Getenv("WORKER_JOB_TEST_DSN")
	if dsn == "" {
		t.Skip("requires WORKER_JOB_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	scope := fmt.Sprintf("wp_job_v2_%d", time.Now().UnixNano())
	schema := pgx.Identifier{scope}.Sanitize()
	defer db.Close(context.Background())
	if _, err := db.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := db.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("clean up migration fixture: %v", err)
		}
	}()
	legacyDSN := dsn + " search_path=" + scope
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		uri, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid PostgreSQL test URL")
		}
		query := uri.Query()
		query.Set("search_path", scope)
		uri.RawQuery = query.Encode()
		legacyDSN = uri.String()
	}
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		for _, sql := range []string{
			"SET LOCAL search_path TO " + schema,
			schemaSQL,
			siteSessionSchema,
			"CREATE TABLE wp_job_migrations (version integer PRIMARY KEY); INSERT INTO wp_job_migrations VALUES (2)",
			legacyPoolRows,
		} {
			if _, err := tx.Exec(ctx, sql); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed v2 database: %v", err)
	}
	store, err := Open(ctx, legacyDSN, "legacy", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const activeID = "00000000-0000-4000-8000-000000000001"
	const completedID = "00000000-0000-4000-8000-000000000002"
	const pool = "ate-system/legacy"
	state, result, err := store.Result(ctx, completedID)
	var body struct{ Version string }
	if err != nil || state != succeeded || json.Unmarshal(result, &body) != nil || body.Version != "v1" {
		t.Fatalf("v2 result changed after migration: %s %s %v", state, result, err)
	}
	view, err := store.Get(ctx, activeID)
	if err != nil || view.State != running || view.Revision != "v1" || view.OccupiedSlots != 1 || len(view.Attempts) != 1 ||
		view.Attempts[0].ActorUID != "legacy-active-uid" {
		t.Fatalf("v2 reservation changed after migration: %+v %v", view, err)
	}
	if _, err := store.pool.Exec(ctx, "INSERT INTO wp_job_pool_limits VALUES ($1, 1)", pool); err != nil {
		t.Fatal(err)
	}
	observeJob(t, ctx, store, "echo", "legacy-worker-uid", "v1", true)
	replay, err := store.Admit(ctx, "echo", "complete", json.RawMessage(`{}`), "")
	if err != nil || replay.ID != completedID || replay.Revision != "v1" {
		t.Fatalf("receipt changed after migration: %+v %v", replay, err)
	}
	queuedRun, err := store.Admit(ctx, "echo", "queued-after-upgrade", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if job, err := store.claim(ctx); err != nil || job.id != "" {
		t.Fatalf("v2 active Run did not retain the pool slot: %s %v", job.id, err)
	}
	active := claim{id: activeID, leaseID: "00000000-0000-4000-8000-000000000003", number: 1}
	if err := store.recordOutcome(ctx, active, outcome{result: json.RawMessage(`{"version":"v1"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.finishAttempt(ctx, active); err != nil {
		t.Fatal(err)
	}
	job, err := store.claim(ctx)
	if err != nil || job.id != queuedRun.ID || job.workerPool != pool || job.revision.TemplateUID != "legacy-template-uid" {
		t.Fatalf("v2 revision or slot was not reusable after release: %s %s %v", job.id, job.workerPool, err)
	}
	if attempt, err := store.startAttempt(ctx, job); err != nil || attempt.Number != 1 {
		t.Fatalf("first reservation after the v2 Run returned its slot: %v %v", attempt, err)
	}
}
