package platform

import (
	"strings"
	"testing"

	"github.com/imprun/worker-platform/internal/apppackage"
	ate "go.ate.dev/substrate/api/ateapi/v1"
)

func TestPackagedRevisionTracksAppAndPreservesItsRuntime(t *testing.T) {
	v1 := "packages.example.test/workers/echo@sha256:" + strings.Repeat("1", 64)
	v2 := "packages.example.test/workers/echo@sha256:" + strings.Repeat("2", 64)
	image := "registry.example.test/workers/runtime@sha256:" + strings.Repeat("3", 64)
	w := Worker{APIVersion: WorkerVersion, Kind: "HTTPWorker", Metadata: ObjectName{Name: "echo", Namespace: "example"},
		Spec: WorkerSpec{Profile: "echo-runtime", AppPackage: v1, Trigger: HTTPTrigger{
			Hostname: "echo.example.test", Path: "/echo",
			Gateway: GatewayRef{ObjectName: ObjectName{Name: "gateway", Namespace: "example"}, SectionName: "web"},
			Router:  RouterRef{ObjectName: ObjectName{Name: "router", Namespace: "ate-system"}, Port: 443},
		}}}
	p := WorkerProfile{APIVersion: WorkerVersion, Kind: "WorkerProfile", Metadata: ObjectName{Name: "echo-runtime", Namespace: "example"},
		Spec: WorkerProfileSpec{Atespace: "example", WorkerPool: ObjectName{Name: "test", Namespace: "ate-system"}, PoolID: "test",
			Architecture: "arm64", SandboxClass: "gvisor", CPU: "20m", Memory: "64Mi", RuntimeImage: image,
			Egress: []EgressRule{{Protocol: "tlsPassthrough", Hostname: "packages.example.test", Port: 443}}}}
	plan := func() ATEPlan {
		t.Helper()
		got, err := BuildATEPlan(w, p)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := plan()
	snapshot, err := newJobRevision(first, "template-v1", p)
	if err != nil {
		t.Fatal(err)
	}
	w.Spec.AppPackage = v2
	second := plan()
	check := func(container *ate.Container, packageRef string) {
		t.Helper()
		if container.GetImage() != image || len(container.GetEnv()) != 1 ||
			container.GetEnv()[0].GetName() != apppackage.ReferenceEnv || container.GetEnv()[0].GetValue() != packageRef {
			t.Fatalf("unexpected packaged container: %v", container)
		}
	}
	check(first.Template.GetContainers()[0], v1)
	check(second.Template.GetContainers()[0], v2)
	if first.Revision == second.Revision || first.Actor.GetMetadata().GetName() == second.Actor.GetMetadata().GetName() {
		t.Fatal("new app digest reused the old revision")
	}
	if !strings.Contains(string(second.Route.Spec), `"replaceFullPath":"/invoke"`) {
		t.Fatal("configured public path must reach the common runtime invoke endpoint")
	}
	retry, err := snapshot.attemptPlan("retry")
	if err != nil {
		t.Fatal(err)
	}
	check(retry.Template.GetContainers()[0], v1)
	w.Spec.AppPackage = v1
	if rolledBack := plan(); rolledBack.Revision != first.Revision || rolledBack.Actor.GetMetadata().GetName() != first.Actor.GetMetadata().GetName() {
		t.Fatal("rollback changed the original package revision")
	}
	p.Spec.Egress = nil
	if _, err := BuildATEPlan(w, p); err == nil {
		t.Fatal("packaged worker could start without the approved registry egress")
	}
}
