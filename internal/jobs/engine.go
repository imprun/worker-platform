package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/imprun/worker-platform/internal/platform"
	ate "go.ate.dev/substrate/api/ateapi/v1"
)

type Runtime interface {
	PrepareAttempt(context.Context, platform.JobRevision, string, func(string) error) (*ate.WorkerAssignment, error)
	AttemptAssigned(context.Context, *ate.ObjectRef, string, string) (bool, error)
	DeleteAttempt(context.Context, platform.JobRevision, string, string) error
}

type InvokeAttempt func(context.Context, *ate.ObjectRef, []byte, int64) (int, []byte, error)

type Engine struct {
	Store   *Store
	Runtime Runtime
	Invoke  InvokeAttempt
}

func (e Engine) Run(ctx context.Context) error {
	busy := make(map[string]struct{})
	finished := make(chan string)
	var running sync.WaitGroup
	defer running.Wait()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for ctx.Err() == nil {
		db, cancel := context.WithTimeout(ctx, 5*time.Second)
		job, err := e.Store.claim(db, slices.Collect(maps.Keys(busy))...)
		cancel()
		if err == nil && job.id != "" {
			busy[job.workerPool] = struct{}{}
			running.Go(func() {
				if err := e.process(ctx, job); err != nil && ctx.Err() == nil && !errors.Is(err, errLeaseLost) {
					slog.Warn("advance Job", "runId", job.id, "error", err)
				}
				select {
				case finished <- job.workerPool:
				case <-ctx.Done():
				}
			})
			continue
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("claim Job", "error", err)
		}
		select {
		case <-ctx.Done():
		case pool := <-finished:
			delete(busy, pool)
		case <-tick.C:
		}
	}
	return nil
}

func (e Engine) process(ctx context.Context, job claim) error {
	work, stop := context.WithCancelCause(ctx)
	stopped := make(chan struct{})
	go e.keepLease(work, stop, job, stopped)
	defer func() {
		stop(nil)
		<-stopped
	}()
	err := e.advance(work, job)
	if errors.Is(err, context.Canceled) && context.Cause(work) != nil {
		return context.Cause(work)
	}
	return err
}

func (e Engine) keepLease(ctx context.Context, stop context.CancelCauseFunc, job claim, done chan<- struct{}) {
	defer close(done)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			db, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := e.Store.renew(db, job)
			cancel()
			if err != nil {
				stop(err)
				return
			}
		}
	}
}

func (e Engine) advance(ctx context.Context, job claim) error {
	if job.state == waitingAuth {
		return e.Store.expireReauth(ctx, job)
	}
	var attempt Attempt
	var err error
	if job.state == queued {
		if !time.Now().Before(job.deadline) {
			return e.Store.expireQueued(ctx, job)
		}
		attempt, err = e.Store.startAttempt(ctx, job)
		if errors.Is(err, errPoolFull) {
			return e.Store.yield(ctx, job, 0)
		}
		job.number = attempt.Number
	} else {
		attempt, err = e.Store.activeAttempt(ctx, job)
	}
	if err != nil {
		return err
	}
	switch attempt.State {
	case starting:
		pending, err := e.prepareAndInvoke(ctx, job, &attempt)
		if pending || err != nil {
			return err
		}
	case running:
		// An expired dispatch may already have reached the app. Reclaim its Actor
		// before another Attempt rather than sending the same dispatch again.
		if err := e.Store.recordOutcome(ctx, job, outcome{reason: "dispatch_lease_expired", retryable: true}); err != nil {
			return err
		}
	case releasing:
	default:
		return errors.New("Run has an invalid active Attempt state")
	}
	cleanup, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = e.Runtime.DeleteAttempt(cleanup, job.revision, attempt.Actor, attempt.ActorUID)
	cancel()
	if err != nil {
		return errors.Join(err, e.Store.yield(ctx, job, 2*time.Second))
	}
	return e.Store.finishAttempt(ctx, job)
}

func (e Engine) prepareAndInvoke(ctx context.Context, job claim, attempt *Attempt) (bool, error) {
	deadline := job.deadline
	if until := attempt.CreatedAt.Add(startupTimeout); until.Before(deadline) {
		deadline = until
	}
	prep, cancel := context.WithDeadline(ctx, deadline)
	assignment, err := e.Runtime.PrepareAttempt(prep, job.revision, attempt.Actor, func(uid string) error {
		if err := e.Store.recordActor(prep, job, uid); err != nil {
			return err
		}
		attempt.ActorUID = uid
		return nil
	})
	cancel()
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, errLeaseLost) {
			return false, err
		}
		slog.Warn("prepare Job Actor", "runId", job.id, "attempt", attempt.Number, "error", err)
		if time.Now().Before(deadline) {
			return true, e.Store.yield(ctx, job, time.Second)
		}
		return false, e.Store.recordOutcome(ctx, job, outcome{reason: "actor_preparation_timeout", retryable: true})
	}
	session, valid, err := e.Store.sessionForAttempt(ctx, job.siteSessionID)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, e.Store.recordOutcome(ctx, job, outcome{reason: "site_session_unavailable"})
	}
	var version int64
	if session != nil {
		version = session.Version
	}
	if !valid {
		return false, e.Store.recordOutcome(ctx, job, expiredSession(version))
	}
	if err := e.Store.startInvocation(ctx, job, attempt.ActorUID, assignment, version); err != nil {
		return false, err
	}
	attempt.WorkerPodUID = assignment.GetWorkerPodUid()
	value := e.invoke(ctx, job, *attempt, session)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, e.Store.recordOutcome(ctx, job, value)
}

func (e Engine) invoke(ctx context.Context, job claim, attempt Attempt, session *siteSession) outcome {
	deadline := time.Now().Add(callTimeout)
	if job.deadline.Before(deadline) {
		deadline = job.deadline
	}
	call, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ref := &ate.ObjectRef{Atespace: attempt.ActorSpace, Name: attempt.Actor}
	request, err := json.Marshal(struct {
		RunID   string          `json:"runId"`
		Attempt int             `json:"attempt"`
		Input   json.RawMessage `json:"input"`
		Session *siteSession    `json:"siteSession,omitempty"`
	}{job.id, attempt.Number, job.input, session})
	if err != nil {
		return outcome{reason: "invalid_input"}
	}
	replies := make(chan outcome, 1)
	go func() {
		code, body, err := e.Invoke(call, ref, request, maxPayload)
		switch {
		case err != nil || code >= 500:
			replies <- outcome{reason: "actor_unavailable", retryable: true}
		case code == http.StatusPreconditionRequired && session != nil && isExpiredSession(body):
			replies <- expiredSession(session.Version)
		case code != http.StatusOK:
			replies <- outcome{reason: "app_rejected_attempt"}
		case len(body) > maxPayload || !validJSON(body):
			replies <- outcome{reason: "invalid_result"}
		default:
			replies <- outcome{result: body}
		}
	}()
	observation := time.NewTicker(time.Second)
	defer observation.Stop()
	for {
		select {
		case value := <-replies:
			return value
		case <-call.Done():
			return outcome{reason: "attempt_deadline", retryable: true}
		case <-observation.C:
			read, cancel := context.WithTimeout(call, 2*time.Second)
			assigned, err := e.Runtime.AttemptAssigned(read, ref, attempt.ActorUID, attempt.WorkerPodUID)
			cancel()
			if err == nil && !assigned {
				return outcome{reason: "worker_assignment_lost", retryable: true}
			}
		}
	}
}

func expiredSession(version int64) outcome {
	return outcome{reason: "site_session_expired", reauthVersion: version}
}

func isExpiredSession(body []byte) bool {
	var event struct {
		Code string `json:"code"`
	}
	return len(body) <= maxPayload && validJSON(body) && json.Unmarshal(body, &event) == nil && event.Code == "site_session_expired"
}
