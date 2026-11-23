package jobs

import (
	"bytes"
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
)

func TestPostgresReauthReleasesSlotAndResumesOnce(t *testing.T) {
	dsn := os.Getenv("WORKER_JOB_TEST_DSN")
	if dsn == "" {
		t.Skip("requires WORKER_JOB_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	scope := fmt.Sprintf("reauth-test-%d", time.Now().UnixNano())
	store, err := Open(ctx, dsn, scope, bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	snapshot := platform.JobRevision{
		TemplateUID: "v1", Profile: platform.WorkerProfile{Spec: platform.WorkerProfileSpec{Atespace: scope}},
	}
	publishJob(t, ctx, store, "echo", scope, "v1", snapshot)
	const ref = "account-a"
	state := func(value string) SiteStateUpdate {
		return SiteStateUpdate{State: json.RawMessage(`{"cookie":"` + value + `"}`), ExpiresAt: time.Now().Add(time.Hour)}
	}
	if _, err := store.CreateSiteSession(ctx, scope, ref, state("private-expired")); err != nil {
		t.Fatal(err)
	}
	run, err := store.Admit(ctx, "echo", "job-with-session", json.RawMessage(`{}`), ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Admit(ctx, "echo", "job-with-session", json.RawMessage(`{}`), ""); !errors.Is(err, ErrInputConflict) {
		t.Fatalf("receipt accepted a different site session: %v", err)
	}
	runtime := &reauthRuntime{t: t, cleanupBlocked: true}
	engine := Engine{Store: store, Runtime: runtime, Invoke: func(_ context.Context, _ *ate.ObjectRef, body []byte, _ int64) (int, []byte, error) {
		var payload struct {
			Attempt int
			Session *siteSession `json:"siteSession"`
		}
		if json.Unmarshal(body, &payload) != nil || payload.Session == nil || payload.Session.Ref != ref {
			return http.StatusBadRequest, nil, nil
		}
		if payload.Attempt == 1 && payload.Session.Version == 1 && string(payload.Session.State) == `{"cookie":"private-expired"}` {
			return http.StatusPreconditionRequired, []byte(`{"code":"site_session_expired"}`), nil
		}
		if payload.Attempt == 2 && payload.Session.Version == 2 && string(payload.Session.State) == `{"cookie":"private-approved"}` {
			return http.StatusOK, []byte(`{"version":"v1"}`), nil
		}
		return http.StatusBadRequest, nil, nil
	}}
	process := func() error {
		t.Helper()
		job, err := store.claim(ctx)
		if err != nil || job.id != run.ID || job.revision.TemplateUID != "v1" {
			t.Fatalf("invalid claim for pinned Run: %s %s %v", job.id, job.revision.TemplateUID, err)
		}
		return engine.process(ctx, job)
	}
	if err := process(); !errors.Is(err, errTestCleanup) {
		t.Fatalf("expected cleanup failure, got %v", err)
	}
	pending, err := store.Get(ctx, run.ID)
	if err != nil || pending.State != releasing || pending.OccupiedSlots != 1 || pending.Reauth != nil {
		t.Fatalf("Run entered approval early: %+v %v", pending, err)
	}
	runtime.cleanupBlocked = false
	if _, err := store.pool.Exec(ctx, "UPDATE wp_job_runs SET due_at=clock_timestamp() WHERE id=$1", run.ID); err != nil {
		t.Fatal(err)
	}
	if err := process(); err != nil {
		t.Fatal(err)
	}
	wait, err := store.Get(ctx, run.ID)
	if err != nil || wait.State != waitingAuth || wait.Reauth == nil || wait.Reauth.State != authPending || wait.OccupiedSlots != 0 ||
		len(wait.Attempts) != 1 || wait.Attempts[0].ActorUID == "" || wait.Attempts[0].SiteSessionVersion != 1 {
		t.Fatalf("invalid waiting Run: %+v %v", wait, err)
	}
	if job, err := store.claim(ctx); err != nil || job.id != "" {
		t.Fatalf("a waiting Run acquired another execution slot: %v", err)
	}
	request := wait.Reauth.ID
	if _, err := store.CompleteReauth(ctx, run.ID, request, state("private-approved")); !errors.Is(err, ErrAuthConflict) {
		t.Fatalf("unstarted request accepted completion: %v", err)
	}
	if receipt, err := store.StartReauth(ctx, run.ID, request); err != nil || receipt.State != authStarted {
		t.Fatalf("cannot start reauth: %+v %v", receipt, err)
	}
	const submitToken = "test-submission-token"
	handler := Handler(store, submitToken, "private-provider-token")
	unauthorized := httptest.NewRequest(http.MethodPost, "/internal/v1/runs/"+run.ID+"/reauth/"+request+"/complete", strings.NewReader("{}"))
	unauthorized.Header.Set("Authorization", "Bearer "+submitToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, unauthorized)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("Job token authorized provider callback: HTTP %d", response.Code)
	}
	var attempts [2]ReauthRequest
	var errs [2]error
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() { attempts[i], errs[i] = store.CompleteReauth(ctx, run.ID, request, state("private-approved")) })
	}
	wg.Wait()
	for i, receipt := range attempts {
		if errs[i] != nil || receipt.ID != request || receipt.State != authCompleted || receipt.SessionVersion != 2 {
			t.Fatalf("duplicate completion changed the receipt: %+v %v", receipt, errs[i])
		}
	}
	if err := process(); err != nil {
		t.Fatal(err)
	}
	done, err := store.Get(ctx, run.ID)
	if err != nil || done.State != succeeded || done.Revision != "v1" || done.OccupiedSlots != 0 || len(done.Attempts) != 2 ||
		done.Attempts[1].ActorUID == done.Attempts[0].ActorUID || done.Attempts[1].SiteSessionVersion != 2 {
		t.Fatalf("invalid resumed Run: %+v %v", done, err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM wp_job_results WHERE run_id=$1", run.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected exactly one result, got %d %v", count, err)
	}
	var sealed []byte
	if err := store.pool.QueryRow(ctx, "SELECT encrypted_state FROM wp_job_site_sessions WHERE namespace=$1", scope).Scan(&sealed); err != nil ||
		len(sealed) == 0 || bytes.Contains(sealed, []byte("private-approved")) {
		t.Fatalf("expected encrypted site state: %v", err)
	}
}

var errTestCleanup = errors.New("test Actor cleanup not finished")

type reauthRuntime struct {
	t              *testing.T
	cleanupBlocked bool
}

func (r *reauthRuntime) PrepareAttempt(_ context.Context, _ platform.JobRevision, name string, record func(string) error) (*ate.WorkerAssignment, error) {
	return assigned(r.t), record(name + "-uid")
}

func (*reauthRuntime) AttemptAssigned(context.Context, *ate.ObjectRef, string, string) (bool, error) {
	return true, nil
}

func (r *reauthRuntime) DeleteAttempt(context.Context, platform.JobRevision, string, string) error {
	if r.cleanupBlocked {
		return errTestCleanup
	}
	return nil
}
