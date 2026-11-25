package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	ate "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var ErrRouteRecoveryUnconfirmed = errors.New("이전 HTTPRoute 복구를 확인하지 못했습니다")

type ATECluster interface {
	Read(context.Context, ObjectName) (*Route, error)
	Publish(context.Context, Route, *Route) error
	CheckWorkerPool(context.Context, WorkerProfile) error
	GatewayProgrammed(context.Context, GatewayRef) (bool, error)
}

type ATEDeployment struct {
	Control       ate.ControlClient
	Cluster       ATECluster
	Ready         func(context.Context, *ate.ObjectRef) (bool, error)
	PollInterval  time.Duration
	WorkerOwner   *metav1.OwnerReference
	BeforePublish func(context.Context) error
}

type ATEDeploymentResult struct {
	Revision  string       `json:"revision"`
	Atespace  string       `json:"atespace"`
	Actor     string       `json:"actor"`
	ActorUID  string       `json:"actorUID"`
	Worker    string       `json:"worker"`
	WorkerPod string       `json:"workerPod"`
	Route     string       `json:"route"`
	Status    string       `json:"status"`
	Job       *JobRevision `json:"-"`
}

func (d ATEDeployment) Deploy(ctx context.Context, w Worker, p WorkerProfile) (ATEDeploymentResult, error) {
	plan, err := d.plan(w, p)
	if err != nil {
		return ATEDeploymentResult{}, err
	}
	before, err := d.Cluster.Read(ctx, w.Metadata)
	if err != nil {
		return ATEDeploymentResult{}, fmt.Errorf("read HTTPRoute: %w", err)
	}
	if before != nil && !plan.Route.owns(*before) {
		return ATEDeploymentResult{}, fmt.Errorf("HTTPRoute %s/%s belongs to another owner", w.Metadata.Namespace, w.Metadata.Name)
	}
	if err := d.Cluster.CheckWorkerPool(ctx, p); err != nil {
		return ATEDeploymentResult{}, err
	}
	templateUID, err := d.ensureTemplate(ctx, plan.Template)
	if err != nil {
		return ATEDeploymentResult{}, err
	}
	actor, err := d.ensureActor(ctx, plan.Actor)
	if err != nil {
		return ATEDeploymentResult{}, err
	}
	ref := objectRef(actor.GetMetadata())
	uid := actor.GetMetadata().GetUid()
	if err := d.ensurePolicy(ctx, ref, plan.Policy, actor.GetStatus().GetState()); err != nil {
		return ATEDeploymentResult{}, err
	}
	assignment, err := d.awaitReady(ctx, plan, p, ref, uid)
	if err != nil {
		return ATEDeploymentResult{}, err
	}
	currentTemplate, err := d.Control.GetActorTemplate(ctx, &ate.GetActorTemplateRequest{ActorTemplate: plan.Actor.GetActorTemplate()})
	if err != nil {
		return ATEDeploymentResult{}, fmt.Errorf("recheck ActorTemplate before publication: %w", err)
	}
	if currentTemplate.GetMetadata().GetUid() != templateUID || !matchesTemplate(plan.Template, currentTemplate) {
		return ATEDeploymentResult{}, errors.New("ActorTemplate changed while the Actor was preparing")
	}
	if d.BeforePublish != nil {
		if err := d.BeforePublish(ctx); err != nil {
			return ATEDeploymentResult{}, err
		}
	}
	if err := d.Cluster.Publish(ctx, plan.Route, before); err != nil {
		return ATEDeploymentResult{}, errors.Join(fmt.Errorf("publish HTTPRoute: %w", err), d.restorePreviousRoute(ctx, plan.Route, before))
	}
	if err := d.awaitRoute(ctx, plan.Route, w.Spec.Trigger.Gateway); err != nil {
		return ATEDeploymentResult{}, errors.Join(err, d.restorePreviousRoute(ctx, plan.Route, before))
	}
	result := ATEDeploymentResult{
		Revision: plan.Revision, Atespace: ref.GetAtespace(), Actor: ref.GetName(), ActorUID: uid,
		Worker: assignment.GetWorker().GetName(), WorkerPod: assignment.GetWorkerPod(),
		Route: w.Metadata.Namespace + "/" + w.Metadata.Name, Status: "published",
	}
	if w.Spec.JobEnabled {
		result.Job, err = newJobRevision(plan, templateUID, p)
	}
	return result, err
}

// restorePreviousRoute는 실패한 시도가 마지막으로 변경한 라우트만 복구한다.
func (d ATEDeployment) restorePreviousRoute(ctx context.Context, candidate Route, previous *Route) error {
	if previous == nil {
		return nil
	}
	delta, err := routePatch(candidate, previous)
	if err != nil || len(delta) == 0 {
		return err
	}
	// 준비 제한 시간이 만료된 뒤에도 라우트 복구가 필요하다.
	restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	current, err := d.Cluster.Read(restore, candidate.Metadata.ObjectName)
	if err != nil {
		return fmt.Errorf("%w: 복구 전 HTTPRoute 조회 실패: %w", ErrRouteRecoveryUnconfirmed, err)
	}
	if current == nil || current.Metadata.UID != previous.Metadata.UID ||
		current.Metadata.Generation != previous.Metadata.Generation+1 || !candidate.owns(*current) {
		return nil
	}
	delta, err = routePatch(candidate, current)
	if err != nil || len(delta) != 0 {
		return err
	}
	if err := d.Cluster.Publish(restore, *previous, current); err != nil {
		return fmt.Errorf("%w: 이전 활성 HTTPRoute 복구 실패: %w", ErrRouteRecoveryUnconfirmed, err)
	}
	return nil
}

func (d ATEDeployment) plan(w Worker, p WorkerProfile) (ATEPlan, error) {
	if d.WorkerOwner == nil {
		return BuildATEPlan(w, p)
	}
	ref := *d.WorkerOwner
	if ref.UID == "" || ref.Kind != "HTTPWorker" || ref.Name != w.Metadata.Name {
		return ATEPlan{}, errors.New("invalid HTTPWorker owner")
	}
	owner := "httpworker:" + w.Metadata.Namespace + "/" + w.Metadata.Name + "/" + string(ref.UID)
	plan, err := buildATEPlan(w, p, owner)
	if err != nil {
		return ATEPlan{}, err
	}
	plan.Route.Metadata.OwnerReferences = []metav1.OwnerReference{ref}
	return plan, nil
}

func objectRef(meta *ate.ResourceMetadata) *ate.ObjectRef {
	return &ate.ObjectRef{Atespace: meta.GetAtespace(), Name: meta.GetName()}
}

func ambiguousCreate(err error) bool {
	switch status.Code(err) {
	case codes.AlreadyExists, codes.Unavailable, codes.DeadlineExceeded:
		return true
	default:
		return false
	}
}

func (d ATEDeployment) ensureTemplate(ctx context.Context, desired *ate.ActorTemplate) (string, error) {
	ref := objectRef(desired.GetMetadata())
	get := &ate.GetActorTemplateRequest{ActorTemplate: ref}
	actual, err := d.Control.GetActorTemplate(ctx, get)
	if status.Code(err) == codes.NotFound {
		actual, err = d.Control.CreateActorTemplate(ctx, &ate.CreateActorTemplateRequest{ActorTemplate: desired})
		if ambiguousCreate(err) && ctx.Err() == nil {
			actual, err = d.Control.GetActorTemplate(ctx, get)
		}
	}
	if err != nil {
		return "", fmt.Errorf("ensure ActorTemplate %s/%s: %w", ref.GetAtespace(), ref.GetName(), err)
	}
	if !matchesTemplate(desired, actual) {
		return "", fmt.Errorf("ActorTemplate %s/%s differs from the planned revision", ref.GetAtespace(), ref.GetName())
	}
	return actual.GetMetadata().GetUid(), nil
}

func matchesTemplate(desired, actual *ate.ActorTemplate) bool {
	if actual.GetMetadata().GetUid() == "" || actual.GetMetadata().GetDeleteTime() != nil ||
		!proto.Equal(objectRef(desired.GetMetadata()), objectRef(actual.GetMetadata())) {
		return false
	}
	value := proto.CloneOf(actual)
	value.Metadata = desired.Metadata
	value.Status = nil
	return proto.Equal(value, desired)
}

func (d ATEDeployment) ensureActor(ctx context.Context, desired *ate.Actor) (*ate.Actor, error) {
	ref := objectRef(desired.GetMetadata())
	get := &ate.GetActorRequest{Actor: ref}
	actual, err := d.Control.GetActor(ctx, get)
	if status.Code(err) == codes.NotFound {
		actual, err = d.Control.CreateActor(ctx, &ate.CreateActorRequest{Actor: desired})
		if ambiguousCreate(err) && ctx.Err() == nil {
			actual, err = d.Control.GetActor(ctx, get)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("ensure Actor %s/%s: %w", ref.GetAtespace(), ref.GetName(), err)
	}
	if !matchesActor(desired, actual) {
		return nil, fmt.Errorf("Actor %s/%s differs from the planned revision", ref.GetAtespace(), ref.GetName())
	}
	return actual, nil
}

func matchesActor(desired, actual *ate.Actor) bool {
	if actual.GetMetadata().GetUid() == "" || actual.GetMetadata().GetDeleteTime() != nil ||
		!proto.Equal(objectRef(desired.GetMetadata()), objectRef(actual.GetMetadata())) {
		return false
	}
	value := proto.CloneOf(actual)
	value.Metadata = desired.Metadata
	value.Status = nil
	return proto.Equal(value, desired)
}

func (d ATEDeployment) ensurePolicy(ctx context.Context, ref *ate.ObjectRef, desired *ate.EgressPolicy, state ate.ActorState) error {
	get := &ate.GetActorEgressPolicyRequest{Actor: ref, Name: desired.GetMetadata().GetName()}
	actual, err := d.Control.GetActorEgressPolicy(ctx, get)
	if status.Code(err) == codes.NotFound && state == ate.ActorState_ACTOR_STATE_SUSPENDED {
		actual, err = d.Control.CreateActorEgressPolicy(ctx, &ate.CreateActorEgressPolicyRequest{Actor: ref, EgressPolicy: desired})
		if ambiguousCreate(err) && ctx.Err() == nil {
			actual, err = d.Control.GetActorEgressPolicy(ctx, get)
		}
	}
	if err != nil {
		return fmt.Errorf("ensure egress policy before using Actor %s/%s: %w", ref.GetAtespace(), ref.GetName(), err)
	}
	if actual.GetMetadata().GetUid() == "" || actual.GetMetadata().GetDeleteTime() != nil ||
		!proto.Equal(objectRef(actual.GetMetadata()), objectRef(desired.GetMetadata())) {
		return errors.New("unexpected Actor egress policy identity")
	}
	value := proto.CloneOf(actual)
	value.Metadata = desired.Metadata
	if !proto.Equal(value, desired) {
		return errors.New("existing Actor egress policy differs from the planned revision")
	}
	return nil
}

func (d ATEDeployment) awaitReady(ctx context.Context, plan ATEPlan, p WorkerProfile, ref *ate.ObjectRef, uid string) (*ate.WorkerAssignment, error) {
	var assignment *ate.WorkerAssignment
	err := d.wait(ctx, func() (bool, error) {
		actual, err := d.Control.GetActor(ctx, &ate.GetActorRequest{Actor: ref})
		if err != nil {
			return false, err
		}
		if actual.GetMetadata().GetUid() != uid || !matchesActor(plan.Actor, actual) {
			return false, errors.New("Actor identity changed before readiness")
		}
		state := actual.GetStatus().GetState()
		switch state {
		case ate.ActorState_ACTOR_STATE_SUSPENDED:
			_, err := d.Control.ResumeActor(ctx, &ate.ResumeActorRequest{Actor: ref})
			if code := status.Code(err); code == codes.ResourceExhausted || code == codes.Unavailable {
				return false, nil
			}
			return false, err
		case ate.ActorState_ACTOR_STATE_RESUMING, ate.ActorState_ACTOR_STATE_SUSPENDING:
			return false, nil
		case ate.ActorState_ACTOR_STATE_RUNNING:
			assignment = actual.GetStatus().GetWorkerAssignment()
			if err := checkAssignment(assignment, p); err != nil {
				return false, err
			}
			return d.Ready(ctx, ref)
		default:
			return false, fmt.Errorf("Actor entered %s", state)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("wait for ready Actor %s/%s: %w", ref.GetAtespace(), ref.GetName(), err)
	}
	return assignment, nil
}

func checkAssignment(a *ate.WorkerAssignment, p WorkerProfile) error {
	pool := p.Spec.WorkerPool
	if a.GetWorker().GetName() == "" || a.GetWorkerPodUid() == "" ||
		a.GetWorkerNamespace() != pool.Namespace || a.GetWorkerPool() != pool.Name {
		return fmt.Errorf("Actor is not assigned to WorkerPool %s/%s", pool.Namespace, pool.Name)
	}
	runtime := a.GetSandboxRuntime()
	if runtime.GetSandboxClass() != p.Spec.SandboxClass {
		return fmt.Errorf("Actor sandbox class %q differs from profile %q", runtime.GetSandboxClass(), p.Spec.SandboxClass)
	}
	for _, attribute := range runtime.GetVersion().GetAttributes() {
		if attribute.GetKey() == "architecture" && attribute.GetValue() == p.Spec.Architecture {
			return nil
		}
	}
	return fmt.Errorf("Actor assignment does not report architecture %q", p.Spec.Architecture)
}

func (d ATEDeployment) awaitRoute(ctx context.Context, route Route, gateway GatewayRef) error {
	err := d.wait(ctx, func() (bool, error) {
		current, err := d.Cluster.Read(ctx, route.Metadata.ObjectName)
		if err != nil {
			return false, err
		}
		if current == nil || !route.owns(*current) {
			return false, errors.New("HTTPRoute disappeared or its ownership changed after publication")
		}
		patch, err := routePatch(route, current)
		if err != nil {
			return false, err
		}
		if len(patch) != 0 {
			return false, errors.New("HTTPRoute target changed before acceptance")
		}
		if !current.acceptedBy(gateway) {
			return false, nil
		}
		return d.Cluster.GatewayProgrammed(ctx, gateway)
	})
	if err != nil {
		return fmt.Errorf("wait for HTTPRoute %s/%s and Gateway programming: %w", route.Metadata.Namespace, route.Metadata.Name, err)
	}
	return nil
}

func (d ATEDeployment) wait(ctx context.Context, check func() (bool, error)) error {
	interval := d.PollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, err := check()
		if ok || err != nil {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
