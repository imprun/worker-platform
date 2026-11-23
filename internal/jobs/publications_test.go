package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/imprun/worker-platform/internal/platform"
)

func observeJob(t *testing.T, ctx context.Context, store *Store, name, uid, priorRevision string, enabled bool) int64 {
	t.Helper()
	epoch, err := store.PublicationEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if current, err := store.ObserveWorker(ctx, name, uid, priorRevision, enabled, epoch); err != nil || !current {
		t.Fatalf("observe publication: current=%v error=%v", current, err)
	}
	return epoch
}

func publishJob(t *testing.T, ctx context.Context, store *Store, name, uid, revision string, snapshot platform.JobRevision) {
	t.Helper()
	epoch := observeJob(t, ctx, store, name, uid, "", true)
	if current, err := store.PublishRevision(ctx, name, uid, revision, epoch, snapshot); err != nil || !current {
		t.Fatalf("publish revision: current=%v error=%v", current, err)
	}
}

func TestPostgresPublicationPreservesActiveAndFencesStaleRollouts(t *testing.T) {
	dsn := os.Getenv("WORKER_JOB_TEST_DSN")
	if dsn == "" {
		t.Skip("requires WORKER_JOB_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	scope := fmt.Sprintf("publication-%d", time.Now().UnixNano())
	store, err := Open(ctx, dsn, scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	const name = "echo"
	admit := func(key, want string, wantErr error) Run {
		t.Helper()
		run, err := store.Admit(ctx, name, key, json.RawMessage(`{}`), "")
		if !errors.Is(err, wantErr) || run.Revision != want {
			t.Fatalf("submit %q: revision=%q error=%v, want %q %v", key, run.Revision, err, want, wantErr)
		}
		return run
	}
	observeJob(t, ctx, store, name, scope, "", true)
	admit("unready", "", ErrNotPublished)
	v1 := platform.JobRevision{TemplateUID: "template-v1"}
	publishJob(t, ctx, store, name, scope, "v1", v1)
	stale := observeJob(t, ctx, store, name, scope, "", true)
	before := admit("before-v2", "v1", nil)
	epoch := observeJob(t, ctx, store, name, scope, "", true)
	v2 := platform.JobRevision{TemplateUID: "template-v2"}
	if ok, err := store.PublishRevision(ctx, name, scope, "v2", epoch, v2); !ok || err != nil {
		t.Fatalf("publish v2: current=%v error=%v", ok, err)
	}
	if ok, err := store.PublishRevision(ctx, name, scope, "v1", stale, v1); ok || err != nil {
		t.Fatalf("stale rollout published v1: current=%v error=%v", ok, err)
	}
	admit("after-v2", "v2", nil)
	if replay := admit("before-v2", "v1", nil); replay.ID != before.ID {
		t.Fatal("replay did not return the same Run ID")
	}
	observeJob(t, ctx, store, name, "", "", false)
	admit("deleted", "", ErrNotFound)
	if ok, err := store.ObserveWorker(ctx, name, scope, "v2", true, epoch); ok || err != nil {
		t.Fatalf("stale observation revived deleted worker: current=%v error=%v", ok, err)
	}
	observeJob(t, ctx, store, name, scope+"-replacement", "", true)
	admit("before-v2", "", ErrNotPublished)
	if old, err := store.Get(ctx, before.ID); err != nil || old.Revision != "v1" {
		t.Fatalf("replacement changed earlier Run: %v %v", old, err)
	}
}
