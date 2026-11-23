package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/imprun/worker-platform/internal/platform"
)

func TestPostgresPoolSlotsAcrossControllers(t *testing.T) {
	dsn := os.Getenv("WORKER_JOB_TEST_DSN")
	if dsn == "" {
		t.Skip("requires WORKER_JOB_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	scope := fmt.Sprintf("test-pool-%d", time.Now().UnixNano())
	var stores [2]*Store
	for i := range stores {
		store, err := Open(ctx, dsn, fmt.Sprintf("%s-%d", scope, i), nil)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = store
		t.Cleanup(store.Close)
	}
	poolA := platform.ObjectName{Namespace: scope, Name: "a"}
	poolB := platform.ObjectName{Namespace: scope, Name: "b"}
	keyA, keyB := scope+"/a", scope+"/b"
	if _, err := stores[0].pool.Exec(ctx, `INSERT INTO wp_job_pool_limits (worker_pool, max_slots) VALUES ($1, 1), ($2, 1)`, keyA, keyB); err != nil {
		t.Fatal(err)
	}
	admit := func(s *Store, pool platform.ObjectName) Run {
		t.Helper()
		uid := s.namespace + "-" + pool.Name
		snapshot := platform.JobRevision{TemplateUID: uid, Profile: platform.WorkerProfile{
			Spec: platform.WorkerProfileSpec{Atespace: s.namespace, WorkerPool: pool},
		}}
		publishJob(t, ctx, s, "echo-"+pool.Name, uid, "v1", snapshot)
		run, err := s.Admit(ctx, "echo-"+pool.Name, "one", json.RawMessage(`{}`), "")
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	type candidate struct {
		store   *Store
		job     claim
		attempt Attempt
		err     error
	}
	var candidates [2]candidate
	for i, s := range stores {
		run := admit(s, poolA)
		job, err := s.claim(ctx)
		if err != nil || job.id != run.ID || job.workerPool != keyA {
			t.Fatalf("unexpected pool A claim: %v %v", job, err)
		}
		candidates[i] = candidate{store: s, job: job}
	}
	var contenders sync.WaitGroup
	start := make(chan struct{})
	for i := range candidates {
		contenders.Go(func() {
			<-start
			c := &candidates[i]
			c.attempt, c.err = c.store.startAttempt(ctx, c.job)
		})
	}
	close(start)
	contenders.Wait()
	winner, waiting := candidates[0], candidates[1]
	if errors.Is(winner.err, errPoolFull) {
		winner, waiting = waiting, winner
	}
	if winner.err != nil || winner.attempt.Number != 1 || !errors.Is(waiting.err, errPoolFull) {
		t.Fatalf("expected exactly one reservation for pool A: %v, %v", winner.err, waiting.err)
	}
	winner.job.number = winner.attempt.Number
	if err := waiting.store.yield(ctx, waiting.job, 0); err != nil {
		t.Fatal(err)
	}
	if err := winner.store.recordOutcome(ctx, winner.job, outcome{reason: "preparation_failed"}); err != nil {
		t.Fatal(err)
	}
	other := admit(waiting.store, poolB)
	job, err := waiting.store.claim(ctx)
	if err != nil || job.id != other.ID || job.workerPool != keyB {
		t.Fatalf("pool A in RELEASING blocked pool B: %v %v", job, err)
	}
	if _, err := waiting.store.startAttempt(ctx, job); err != nil {
		t.Fatal(err)
	}
	if job, err := waiting.store.claim(ctx); err != nil || job.id != "" {
		t.Fatalf("releasing pool A was eligible before cleanup: %v %v", job, err)
	}
	if err := winner.store.finishAttempt(ctx, winner.job); err != nil {
		t.Fatal(err)
	}
	job, err = waiting.store.claim(ctx)
	if err != nil || job.id != waiting.job.id {
		t.Fatalf("waiting Run was not selected after slot return: %v %v", job, err)
	}
	if attempt, err := waiting.store.startAttempt(ctx, job); err != nil || attempt.Number != 1 {
		t.Fatalf("waiting Run consumed an Attempt before slot return: %v %v", attempt, err)
	}
}
