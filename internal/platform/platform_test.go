package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func example(t *testing.T) Worker {
	t.Helper()
	file, err := os.Open("../../examples/echo/worker.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	w, err := ReadWorker(file)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestPlanOverwritesTargetAndPinsRelease(t *testing.T) {
	w := example(t)
	plan, err := BuildPlan(w)
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Rules []struct {
			Matches []struct{ Method string }
			Filters []struct {
				Type                  string
				RequestHeaderModifier struct {
					Set []struct{ Name, Value string }
				}
			}
		}
	}
	if err := json.Unmarshal(plan.Route.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	filter := spec.Rules[0].Filters[0]
	header := filter.RequestHeaderModifier.Set[0]
	if filter.Type != "RequestHeaderModifier" || header.Name != "ate-target-actor" ||
		header.Value != w.Spec.Atespace+"/"+plan.Task.GetMetadata().GetName() || spec.Rules[0].Matches[0].Method != "POST" {
		t.Fatalf("unexpected trigger %s", plan.Route.Spec)
	}
	w.Spec.Trigger.Path = "/new"
	updated, err := BuildPlan(w)
	if err != nil || updated.Task.GetMetadata().GetName() != plan.Task.GetMetadata().GetName() || bytes.Equal(updated.Route.Spec, plan.Route.Spec) {
		t.Fatalf("a route-only change should reuse the release: %v", err)
	}
	w.Spec.Image = strings.ReplaceAll(w.Spec.Image, strings.Repeat("1", 64), strings.Repeat("2", 64))
	updated, err = BuildPlan(w)
	if err != nil || updated.Task.GetMetadata().GetName() == plan.Task.GetMetadata().GetName() {
		t.Fatalf("a new digest must create a different AX Task: %v", err)
	}
	w.Metadata.Namespace = "another-team"
	differentOwner, err := BuildPlan(w)
	if err != nil || differentOwner.Task.GetMetadata().GetName() == updated.Task.GetMetadata().GetName() {
		t.Fatalf("tasks sharing an Atespace must include their owner in the name: %v", err)
	}
}

func TestWorkerRejectsAmbiguousManifests(t *testing.T) {
	data, err := os.ReadFile("../../examples/echo/worker.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, changed := range map[string]string{
		"unknown key":     strings.Replace(string(data), "  cpu: 100m", "  cores: 100m", 1),
		"second document": string(data) + "\n---\n{}\n",
		"unpinned image":  strings.Replace(string(data), "@sha256:"+strings.Repeat("1", 64), ":latest", 1),
		"wildcard":        strings.Replace(string(data), "echo.example.test", "'*.example.test'", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadWorker(strings.NewReader(changed)); err == nil {
				t.Fatal("manifest was accepted")
			}
		})
	}
}

func TestDeployWaitsForReadinessAndReusesRollbackTask(t *testing.T) {
	v1 := example(t)
	v2 := v1
	v2.Spec.Image = strings.ReplaceAll(v2.Spec.Image, strings.Repeat("1", 64), strings.Repeat("2", 64))
	backend := &fakeAX{tasks: make(map[string]*ax.Task), resumed: make(chan string, 2), immediate: true}
	store := &fakeRoutes{}
	deploy := Deployment{AX: grpcAX(t, backend), Routes: store, PollInterval: time.Millisecond}
	deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	first, err := deploy.Deploy(deadline, v1)
	if err != nil {
		t.Fatal(err)
	}
	<-backend.resumed
	backend.mu.Lock()
	backend.immediate = false
	backend.mu.Unlock()
	ctx, cancel := context.WithCancel(deadline)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := deploy.Deploy(ctx, v2); done <- err }()
	var secondName string
	select {
	case secondName = <-backend.resumed:
	case <-deadline.Done():
		t.Fatal("v2 did not start")
	}
	if store.publishes != 1 {
		t.Fatal("a route was published while its Actor was unready")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) && status.Code(err) != codes.Canceled {
		t.Fatalf("expected the readiness wait to stop after cancellation, got %v", err)
	}
	backend.mu.Lock()
	markReady(backend.tasks[secondName])
	backend.mu.Unlock()
	second, err := deploy.Deploy(deadline, v2)
	if err != nil || second.GetMetadata().GetName() != secondName || store.publishes != 2 {
		t.Fatalf("failed to publish the ready v2 Actor: %v", err)
	}
	restored, err := deploy.Deploy(deadline, v1)
	backend.mu.Lock()
	created := len(backend.tasks)
	backend.mu.Unlock()
	if err != nil || !proto.Equal(restored, first) || created != 2 || store.publishes != 3 {
		t.Fatalf("rollback failed to reuse the original v1 Actor: %v", err)
	}
}

func TestRoutePatchChecksObjectIdentityAndOwnership(t *testing.T) {
	p, err := BuildPlan(example(t))
	if err != nil {
		t.Fatal(err)
	}
	current := p.Route
	current.Metadata.UID, current.Metadata.Generation = "uid", 9
	patch, err := routePatch(p.Route, &current)
	if err != nil || len(patch) != 0 {
		t.Fatalf("unchanged spec should be a no-op: %s %v", patch, err)
	}
	current.Spec = json.RawMessage(`{"rules":[]}`)
	patch, err = routePatch(p.Route, &current)
	if err != nil || !bytes.Contains(patch, []byte(`"path":"/metadata/uid","value":"uid"`)) ||
		!bytes.Contains(patch, []byte(`"path":"/metadata/generation","value":9`)) ||
		!bytes.Contains(patch, []byte(`"path":"/metadata/annotations/workerplatform.imprun.dev~1owner"`)) {
		t.Fatalf("missing a compare-and-swap guard: %s %v", patch, err)
	}
}

func grpcAX(t *testing.T, backend *fakeAX) ax.AXClient {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	ax.RegisterAXServer(server, backend)
	t.Cleanup(server.Stop)
	go server.Serve(listener)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return ax.NewAXClient(conn)
}

type fakeAX struct {
	ax.UnimplementedAXServer
	mu        sync.Mutex
	tasks     map[string]*ax.Task
	immediate bool
	resumed   chan string
}

func (s *fakeAX) GetTask(_ context.Context, req *ax.GetTaskRequest) (*ax.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[req.GetName()]
	if task == nil {
		return nil, status.Error(codes.NotFound, "task missing")
	}
	return proto.CloneOf(task), nil
}

func (s *fakeAX) CreateTask(_ context.Context, req *ax.CreateTaskRequest) (*ax.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := proto.CloneOf(req.GetTask())
	task.Status = &ax.TaskStatus{Phase: "Suspended"}
	s.tasks[task.GetMetadata().GetName()] = task
	return proto.CloneOf(task), nil
}

func (s *fakeAX) ResumeTask(_ context.Context, req *ax.ResumeTaskRequest) (*ax.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[req.GetName()]
	task.Status.Phase = "Running"
	if s.immediate {
		markReady(task)
	}
	s.resumed <- req.GetName()
	return proto.CloneOf(task), nil
}

func markReady(task *ax.Task) {
	task.Status.Actor = task.GetMetadata().GetName()
	task.Status.ActorUid = "uid-" + task.Status.Actor
	task.Status.Conditions = []*ax.Condition{{Type: "Ready", Status: "True"}}
}

type fakeRoutes struct {
	current   *Route
	publishes int
}

func (s *fakeRoutes) Read(context.Context, ObjectName) (*Route, error) { return s.current, nil }

func (s *fakeRoutes) Publish(_ context.Context, route Route, before *Route) error {
	if before != s.current {
		return errors.New("stale route")
	}
	s.publishes++
	s.current = &route
	return nil
}
