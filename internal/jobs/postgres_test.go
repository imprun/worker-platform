package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imprun/worker-platform/internal/platform"
	ate "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestPostgresRunRetryKeepsRevisionAndOneResult(t *testing.T) {
	dsn := os.Getenv("WORKER_JOB_TEST_DSN")
	if dsn == "" {
		t.Skip("requires WORKER_JOB_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	scope := fmt.Sprintf("test-%d", time.Now().UnixNano())
	store, err := Open(ctx, dsn, scope)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	worker := Publication{WorkerUID: scope + "-uid", Revision: "v1"}
	for _, version := range []string{"v1", "v2"} {
		rev := platform.JobRevision{
			TemplateUID: version,
			Template:    json.RawMessage(`{"containers":[{"image":"echo:` + version + `"}]}`),
			Profile:     platform.WorkerProfile{Spec: platform.WorkerProfileSpec{Atespace: scope}},
		}
		if err := store.RecordRevision(ctx, worker.WorkerUID, version, rev); err != nil {
			t.Fatal(err)
		}
	}

	token := strings.Repeat("t", 32)
	handler := Handler(store, func(context.Context, string) (Publication, error) { return worker, nil }, token)
	submit := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/workers/echo/runs", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", "same-key")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	var receipts [4]*httptest.ResponseRecorder
	var wg sync.WaitGroup
	for i := range receipts {
		wg.Go(func() { receipts[i] = submit(`{"message":"original","n":1}`) })
	}
	wg.Wait()
	var first struct{ RunID, Revision string }
	if err := json.Unmarshal(receipts[0].Body.Bytes(), &first); err != nil || first.RunID == "" || first.Revision != "v1" {
		t.Fatalf("invalid first receipt: %s, %v", receipts[0].Body, err)
	}
	for _, response := range receipts {
		if response.Code != http.StatusAccepted || response.Body.String() != receipts[0].Body.String() {
			t.Fatalf("concurrent acceptance returned a different receipt: %d %s", response.Code, response.Body)
		}
	}
	worker.Revision = "v2"
	if response := submit(`{"n":1.0,"message":"original"}`); response.Code != http.StatusAccepted || response.Body.String() != receipts[0].Body.String() {
		t.Fatalf("replay switched to v2: %d %s", response.Code, response.Body)
	}
	if response := submit(`{"message":"changed","n":1}`); response.Code != http.StatusConflict {
		t.Fatalf("changed input was accepted: %d %s", response.Code, response.Body)
	}
	if response := submit(`{"message":"changed","message":"original","n":1}`); response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate JSON member was accepted: %d %s", response.Code, response.Body)
	}
	if state, value, err := store.Result(ctx, first.RunID); err != nil || state != queued || value != nil {
		t.Fatalf("queued Run exposed a result: %s %s %v", state, value, err)
	}

	oldLease, err := store.claim(ctx)
	if err != nil || oldLease.id != first.RunID || oldLease.revision.TemplateUID != "v1" {
		t.Fatalf("unexpected claim: %s %s %v", oldLease.id, oldLease.revision.TemplateUID, err)
	}
	old, err := store.startAttempt(ctx, oldLease)
	if err != nil {
		t.Fatal(err)
	}
	oldLease.number = old.Number
	if err := store.recordActor(ctx, oldLease, "actor-before-crash"); err != nil {
		t.Fatal(err)
	}
	if err := store.startInvocation(ctx, oldLease, "actor-before-crash", assigned(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, "UPDATE wp_job_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", first.RunID); err != nil {
		t.Fatal(err)
	}

	reclaimed, err := store.claim(ctx)
	if err != nil || reclaimed.state != running || reclaimed.leaseID == oldLease.leaseID {
		t.Fatalf("expired dispatch not reclaimed: %s %v", reclaimed.state, err)
	}
	if err := store.recordOutcome(ctx, oldLease, outcome{result: json.RawMessage(`{"stale":true}`)}); !errors.Is(err, errLeaseLost) {
		t.Fatalf("expired dispatch accepted a result: %v", err)
	}
	runtime := &attemptRuntime{t: t}
	calls := 0
	engine := Engine{Store: store, Runtime: runtime, Invoke: func(_ context.Context, ref *ate.ObjectRef, body []byte, _ int64) (int, []byte, error) {
		calls++
		var payload struct {
			RunID   string
			Attempt int
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.RunID != first.RunID || payload.Attempt != 2 || ref.GetName() == old.Actor {
			return http.StatusBadRequest, nil, nil
		}
		return http.StatusOK, []byte(`{"version":"v1"}`), nil
	}}
	if err := engine.process(ctx, reclaimed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, "UPDATE wp_job_runs SET due_at=clock_timestamp() WHERE id=$1", first.RunID); err != nil {
		t.Fatal(err)
	}
	retry, err := store.claim(ctx)
	if err != nil || retry.state != queued || retry.revision.TemplateUID != "v1" {
		t.Fatalf("retry changed the pinned revision: %s %v", retry.revision.TemplateUID, err)
	}
	if err := engine.process(ctx, retry); err != nil {
		t.Fatal(err)
	}
	view, err := store.Get(ctx, first.RunID)
	if err != nil || view.State != succeeded || view.Revision != "v1" || len(view.Attempts) != 2 ||
		view.Attempts[0].ActorUID != "actor-before-crash" || view.Attempts[0].State != failed ||
		view.Attempts[1].ActorUID != "actor-after-crash" || view.Attempts[1].State != succeeded ||
		calls != 1 || len(runtime.deleted) != 2 {
		t.Fatalf("unexpected Run or runtime side effects: %+v calls=%d deleted=%v error=%v", view, calls, runtime.deleted, err)
	}
	state, result, err := store.Result(ctx, first.RunID)
	var body map[string]string
	if err != nil || state != succeeded || json.Unmarshal(result, &body) != nil || body["version"] != "v1" {
		t.Fatalf("invalid final result: %s %s %v", state, result, err)
	}
	var runs, count, number int
	err = store.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM wp_job_runs WHERE namespace=$1), count(*), max(attempt_no)
		FROM wp_job_results WHERE run_id=$2
	`, scope, first.RunID).Scan(&runs, &count, &number)
	if err != nil || runs != 1 || count != 1 || number != 2 {
		t.Fatalf("runs=%d, results=%d, attempt=%d, error=%v", runs, count, number, err)
	}
}

type attemptRuntime struct {
	t       *testing.T
	deleted []string
}

func (r *attemptRuntime) PrepareAttempt(_ context.Context, revision platform.JobRevision, _ string, record func(string) error) (*ate.WorkerAssignment, error) {
	if revision.TemplateUID != "v1" {
		return nil, errors.New("expected pinned v1")
	}
	return assigned(r.t), record("actor-after-crash")
}

func (*attemptRuntime) AttemptAssigned(context.Context, *ate.ObjectRef, string, string) (bool, error) {
	return true, nil
}

func (r *attemptRuntime) DeleteAttempt(_ context.Context, _ platform.JobRevision, _ string, uid string) error {
	r.deleted = append(r.deleted, uid)
	return nil
}

func assigned(t *testing.T) *ate.WorkerAssignment {
	t.Helper()
	assignment := new(ate.WorkerAssignment)
	if err := protojson.Unmarshal([]byte(`{"worker":{"name":"test-worker"},"workerPodUid":"test-pod"}`), assignment); err != nil {
		t.Fatal(err)
	}
	return assignment
}
