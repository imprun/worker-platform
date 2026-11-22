package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	ate "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// JobRevision is the immutable ATE input saved before publishing a runnable revision.
type JobRevision struct {
	Profile     WorkerProfile   `json:"profile"`
	Template    json.RawMessage `json:"template"`
	TemplateUID string          `json:"templateUID"`
	Policy      json.RawMessage `json:"egressPolicy"`
}

func newJobRevision(plan ATEPlan, uid string, profile WorkerProfile) (*JobRevision, error) {
	template, err := protojson.Marshal(plan.Template)
	if err != nil {
		return nil, err
	}
	policy, err := protojson.Marshal(plan.Policy)
	if err != nil {
		return nil, err
	}
	return &JobRevision{Profile: profile, Template: template, TemplateUID: uid, Policy: policy}, nil
}

func (r JobRevision) attemptPlan(name string) (ATEPlan, error) {
	template := new(ate.ActorTemplate)
	if err := protojson.Unmarshal(r.Template, template); err != nil {
		return ATEPlan{}, err
	}
	ref := objectRef(template.GetMetadata())
	if r.TemplateUID == "" || ref.GetAtespace() != r.Profile.Spec.Atespace || ref.GetName() == "" {
		return ATEPlan{}, errors.New("invalid pinned ActorTemplate")
	}
	policy := new(ate.EgressPolicy)
	if err := protojson.Unmarshal(r.Policy, policy); err != nil {
		return ATEPlan{}, err
	}
	return ATEPlan{Template: template, Policy: policy, Actor: &ate.Actor{
		Metadata:      &ate.ResourceMetadata{Atespace: ref.GetAtespace(), Name: name},
		ActorTemplate: ref,
	}}, nil
}

func (d ATEDeployment) PrepareAttempt(ctx context.Context, revision JobRevision, name string, recordUID func(string) error) (*ate.WorkerAssignment, error) {
	plan, err := revision.attemptPlan(name)
	if err != nil {
		return nil, err
	}
	if err := d.Cluster.CheckWorkerPool(ctx, revision.Profile); err != nil {
		return nil, err
	}
	template, err := d.Control.GetActorTemplate(ctx, &ate.GetActorTemplateRequest{ActorTemplate: plan.Actor.GetActorTemplate()})
	if err != nil {
		return nil, fmt.Errorf("read pinned ActorTemplate: %w", err)
	}
	if template.GetMetadata().GetUid() != revision.TemplateUID || !matchesTemplate(plan.Template, template) {
		return nil, errors.New("pinned ActorTemplate identity changed")
	}
	actor, err := d.ensureActor(ctx, plan.Actor)
	if err != nil {
		return nil, err
	}
	uid := actor.GetMetadata().GetUid()
	if err := recordUID(uid); err != nil {
		return nil, err
	}
	ref := objectRef(actor.GetMetadata())
	if err := d.ensurePolicy(ctx, ref, plan.Policy, actor.GetStatus().GetState()); err != nil {
		return nil, err
	}
	return d.awaitReady(ctx, plan, revision.Profile, ref, uid)
}

func (d ATEDeployment) AttemptAssigned(ctx context.Context, ref *ate.ObjectRef, uid, workerPodUID string) (bool, error) {
	actor, err := d.Control.GetActor(ctx, &ate.GetActorRequest{Actor: ref})
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s := actor.GetStatus()
	return actor.GetMetadata().GetUid() == uid && s.GetState() == ate.ActorState_ACTOR_STATE_RUNNING &&
		s.GetWorkerAssignment().GetWorkerPodUid() == workerPodUID, nil
}

func (d ATEDeployment) DeleteAttempt(ctx context.Context, revision JobRevision, name, uid string) error {
	plan, err := revision.attemptPlan(name)
	if err != nil {
		return err
	}
	ref := objectRef(plan.Actor.GetMetadata())
	actor, err := d.Control.GetActor(ctx, &ate.GetActorRequest{Actor: ref})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	if err != nil {
		return err
	}
	actualUID := actor.GetMetadata().GetUid()
	if actualUID == "" || (uid != "" && uid != actualUID) ||
		!matchesActor(plan.Actor, actor) {
		return errors.New("refusing cleanup of a different Actor")
	}
	_, err = d.Control.DeleteActor(ctx, &ate.DeleteActorRequest{Actor: ref, Options: &ate.DeleteOptions{Uid: actualUID}})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	if err != nil {
		return err
	}
	return d.wait(ctx, func() (bool, error) {
		_, err := d.Control.GetActor(ctx, &ate.GetActorRequest{Actor: ref})
		if status.Code(err) == codes.NotFound {
			return true, nil
		}
		return false, err
	})
}
