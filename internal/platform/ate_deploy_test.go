package platform

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	ate "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ateExample(t *testing.T) (Worker, WorkerProfile) {
	t.Helper()
	file, err := os.Open("../../examples/echo/ate-worker.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	w, err := ReadWorker(file)
	if err != nil {
		t.Fatal(err)
	}
	file, err = os.Open("../../examples/echo/ate-profile.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	p, err := ReadWorkerProfile(file)
	if err != nil {
		t.Fatal(err)
	}
	return w, p
}

func TestATERevisionUsesEffectiveExecutionSettings(t *testing.T) {
	w, p := ateExample(t)
	p.Spec.CPU = "1"
	first, err := BuildATEPlan(w, p)
	if err != nil {
		t.Fatal(err)
	}
	p.Spec.CPU = "1000m"
	w.Spec.Trigger.Hostname = "other.example.test"
	next, err := BuildATEPlan(w, p)
	if err != nil || next.Revision != first.Revision {
		t.Fatalf("equivalent execution settings changed revision: %s, %v", next.Revision, err)
	}
	p.Spec.Egress = []EgressRule{{Protocol: "tlsPassthrough", Hostname: "example.com", Port: 443}}
	next, err = BuildATEPlan(w, p)
	if err != nil || next.Revision == first.Revision {
		t.Fatalf("egress policy must be part of the revision: %s, %v", next.Revision, err)
	}
	p.Metadata.Namespace = "other"
	if _, err := BuildATEPlan(w, p); err == nil {
		t.Fatal("accepted a profile from another namespace")
	}
}

func TestATEDirectDeploymentGatesRouteAndReusesRevisions(t *testing.T) {
	w, p := ateExample(t)
	control := &ateFixture{
		templates: map[string]*ate.ActorTemplate{},
		actors:    map[string]*ate.Actor{},
		policies:  map[string]*ate.EgressPolicy{},
	}
	cluster := &ateClusterFixture{gateway: w.Spec.Trigger.Gateway}
	probeError := errors.New("application is not ready")
	failProbe := false
	d := ATEDeployment{
		Control: control, Cluster: cluster, PollInterval: time.Millisecond,
		Ready: func(context.Context, *ate.ObjectRef) (bool, error) {
			if failProbe {
				return false, probeError
			}
			return true, nil
		},
	}
	first, err := d.Deploy(t.Context(), w, p)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(control.mutations, []string{"template", "actor", "policy", "resume"}) || cluster.publications != 1 {
		t.Fatalf("unexpected provisioning: %v; publications %d", control.mutations, cluster.publications)
	}
	previous := string(cluster.route.Spec)
	upgrade := w
	upgrade.Spec.Image = strings.Replace(w.Spec.Image, strings.Repeat("1", 64), strings.Repeat("2", 64), 1)
	failProbe = true
	if _, err := d.Deploy(t.Context(), upgrade, p); !errors.Is(err, probeError) {
		t.Fatalf("expected readiness failure, got %v", err)
	}
	if string(cluster.route.Spec) != previous || cluster.publications != 1 {
		t.Fatal("failed rollout changed the current route")
	}
	failProbe = false
	stale := errors.New("newer generation")
	d.BeforePublish = func(context.Context) error { return stale }
	if _, err := d.Deploy(t.Context(), upgrade, p); !errors.Is(err, stale) {
		t.Fatalf("expected publication guard to reject a ready candidate, got %v", err)
	}
	if string(cluster.route.Spec) != previous || cluster.publications != 1 {
		t.Fatal("superseded rollout changed the current route")
	}
	d.BeforePublish = nil
	upgraded, err := d.Deploy(t.Context(), upgrade, p)
	if err != nil || upgraded.ActorUID == first.ActorUID {
		t.Fatalf("upgrade did not select another Actor: %+v, %v", upgraded, err)
	}
	rolledBack, err := d.Deploy(t.Context(), w, p)
	if err != nil || rolledBack.ActorUID != first.ActorUID || string(cluster.route.Spec) != previous {
		t.Fatalf("rollback did not reuse the original Actor: %+v, %v", rolledBack, err)
	}
	if len(control.mutations) != 8 || cluster.publications != 3 {
		t.Fatalf("reusing revisions performed unexpected mutations: %v; publications %d", control.mutations, cluster.publications)
	}

	cluster.route.Metadata.Annotations[OwnerAnnotation] = w.Metadata.Namespace + "/" + w.Metadata.Name
	if _, err := d.Deploy(t.Context(), w, p); err == nil || len(control.mutations) != 8 {
		t.Fatalf("ATE deployment accepted a route owned by another deployer: %v", err)
	}
}

func TestHTTPWorkerUIDIsolatesRevisions(t *testing.T) {
	w, p := ateExample(t)
	local, err := BuildATEPlan(w, p)
	if err != nil {
		t.Fatal(err)
	}
	ref := &metav1.OwnerReference{
		APIVersion: "workerplatform.imprun.dev/v1alpha3", Kind: w.Kind, Name: w.Metadata.Name, UID: "worker-1",
	}
	d := ATEDeployment{WorkerOwner: ref}
	first, err := d.plan(w, p)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == local.Revision || first.Route.owns(local.Route) ||
		len(first.Route.Metadata.OwnerReferences) != 1 || first.Route.Metadata.OwnerReferences[0].UID != ref.UID {
		t.Fatal("Kubernetes-owned route or Actor overlaps the file-based deployment")
	}
	ref.UID = "worker-2"
	next, err := d.plan(w, p)
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision == first.Revision || next.Route.owns(first.Route) {
		t.Fatal("a new HTTPWorker UID inherited an earlier deployment")
	}
}

func TestATERouteRequiresCurrentGatewayListenerStatus(t *testing.T) {
	w, p := ateExample(t)
	plan, err := BuildATEPlan(w, p)
	if err != nil {
		t.Fatal(err)
	}
	c := &ateClusterFixture{gateway: w.Spec.Trigger.Gateway}
	if err := c.Publish(t.Context(), plan.Route, nil); err != nil {
		t.Fatal(err)
	}
	if !c.route.acceptedBy(c.gateway) {
		t.Fatal("current Gateway listener conditions should satisfy publication")
	}
	c.route.Metadata.Generation++
	if c.route.acceptedBy(c.gateway) {
		t.Fatal("stale conditions accepted")
	}
	c.route.Status.Parents[0].Conditions[0].ObservedGeneration++
	c.route.Status.Parents[0].Conditions[1].ObservedGeneration++
	c.route.Status.Parents[0].ParentRef.SectionName = "other"
	if c.route.acceptedBy(c.gateway) {
		t.Fatal("a different listener's conditions accepted")
	}
}

type ateFixture struct {
	ate.ControlClient
	templates map[string]*ate.ActorTemplate
	actors    map[string]*ate.Actor
	policies  map[string]*ate.EgressPolicy
	mutations []string
}

func (f *ateFixture) GetActorTemplate(_ context.Context, r *ate.GetActorTemplateRequest, _ ...grpc.CallOption) (*ate.ActorTemplate, error) {
	if value := f.templates[r.GetActorTemplate().GetName()]; value != nil {
		return proto.CloneOf(value), nil
	}
	return nil, status.Error(codes.NotFound, "template")
}

func (f *ateFixture) CreateActorTemplate(_ context.Context, r *ate.CreateActorTemplateRequest, _ ...grpc.CallOption) (*ate.ActorTemplate, error) {
	value := proto.CloneOf(r.GetActorTemplate())
	value.Metadata.Uid = "template-" + value.Metadata.Name
	f.templates[value.Metadata.Name] = value
	f.mutations = append(f.mutations, "template")
	return proto.CloneOf(value), nil
}

func (f *ateFixture) GetActor(_ context.Context, r *ate.GetActorRequest, _ ...grpc.CallOption) (*ate.Actor, error) {
	if value := f.actors[r.GetActor().GetName()]; value != nil {
		return proto.CloneOf(value), nil
	}
	return nil, status.Error(codes.NotFound, "actor")
}

func (f *ateFixture) CreateActor(_ context.Context, r *ate.CreateActorRequest, _ ...grpc.CallOption) (*ate.Actor, error) {
	value := proto.CloneOf(r.GetActor())
	value.Metadata.Uid = "actor-" + value.Metadata.Name
	value.Status = &ate.ActorStatus{State: ate.ActorState_ACTOR_STATE_SUSPENDED}
	f.actors[value.Metadata.Name] = value
	f.mutations = append(f.mutations, "actor")
	return nil, status.Error(codes.Unavailable, "committed before response was lost")
}

func (f *ateFixture) GetActorEgressPolicy(_ context.Context, r *ate.GetActorEgressPolicyRequest, _ ...grpc.CallOption) (*ate.EgressPolicy, error) {
	if value := f.policies[r.GetActor().GetName()]; value != nil {
		return proto.CloneOf(value), nil
	}
	return nil, status.Error(codes.NotFound, "policy")
}

func (f *ateFixture) CreateActorEgressPolicy(_ context.Context, r *ate.CreateActorEgressPolicyRequest, _ ...grpc.CallOption) (*ate.EgressPolicy, error) {
	value := proto.CloneOf(r.GetEgressPolicy())
	value.Metadata.Uid = "policy-" + r.GetActor().GetName()
	f.policies[r.GetActor().GetName()] = value
	f.mutations = append(f.mutations, "policy")
	return proto.CloneOf(value), nil
}

func (f *ateFixture) ResumeActor(_ context.Context, r *ate.ResumeActorRequest, _ ...grpc.CallOption) (*ate.ResumeActorResponse, error) {
	name := r.GetActor().GetName()
	if f.policies[name] == nil {
		return nil, status.Error(codes.FailedPrecondition, "resume before policy")
	}
	value := f.actors[name]
	assignment := &ate.WorkerAssignment{}
	if err := protojson.Unmarshal([]byte(`{
		"worker":{"name":"worker"}, "workerPool":"echo-arm64-dev", "workerNamespace":"ate-system",
		"workerPod":"test-worker", "workerPodUid":"pod-uid",
		"sandboxRuntime":{"sandboxClass":"gvisor","version":{"attributes":[{"key":"architecture","value":"arm64"}]}}
	}`), assignment); err != nil {
		return nil, err
	}
	value.Status = &ate.ActorStatus{State: ate.ActorState_ACTOR_STATE_RUNNING, WorkerAssignment: assignment}
	f.mutations = append(f.mutations, "resume")
	return &ate.ResumeActorResponse{Actor: proto.CloneOf(value), Resumed: true}, nil
}

type ateClusterFixture struct {
	gateway      GatewayRef
	route        *Route
	publications int
}

func (*ateClusterFixture) CheckWorkerPool(context.Context, WorkerProfile) error { return nil }
func (*ateClusterFixture) GatewayProgrammed(context.Context, GatewayRef) (bool, error) {
	return true, nil
}
func (f *ateClusterFixture) Read(context.Context, ObjectName) (*Route, error) { return f.route, nil }
func (f *ateClusterFixture) Publish(_ context.Context, route Route, _ *Route) error {
	f.publications++
	generation := int64(f.publications)
	route.Metadata.Generation = generation
	route.Status = &RouteStatus{Parents: []RouteParentStatus{{
		ParentRef: RouteParentRef{Name: f.gateway.Name, Namespace: f.gateway.Namespace, SectionName: f.gateway.SectionName},
		Conditions: []Condition{
			{Type: "Accepted", Status: "True", ObservedGeneration: generation},
			{Type: "ResolvedRefs", Status: "True", ObservedGeneration: generation},
		},
	}}}
	f.route = &route
	return nil
}
