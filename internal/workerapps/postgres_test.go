package workerapps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/imprun/worker-platform/internal/platform"
	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestWorkerAppPostgresHTTP(t *testing.T) {
	dsn := os.Getenv("WORKER_APP_TEST_DSN")
	if dsn == "" {
		t.Skip("WORKER_APP_TEST_DSN에 격리된 테스트 데이터베이스를 지정하세요")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("테스트 데이터베이스 연결 설정 오류")
	}
	t.Cleanup(pool.Close)
	const namespace = "worker-dev"
	var identity [16]byte
	rand.Read(identity[:])
	organization := "worker-app-http-test-" + hex.EncodeToString(identity[:])
	profile := platform.WorkerProfileSpec{
		Atespace: "worker-dev", WorkerPool: platform.ObjectName{Name: "arm", Namespace: "ate"},
		PoolID: "arm", Architecture: "arm64", SandboxClass: "gvisor", CPU: "20m", Memory: "64Mi",
		RuntimeImage: "registry.example.test/runtime@sha256:" + strings.Repeat("1", 64),
		Egress:       []platform.EgressRule{{Protocol: "tlsPassthrough", Hostname: "registry.example.test", Port: 443}},
	}
	profileSpec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&profile)
	if err != nil {
		t.Fatal(err)
	}
	kube := fake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": workerResource.GroupVersion().String(), "kind": "WorkerProfile",
		"metadata": map[string]any{"name": "arm", "namespace": namespace}, "spec": profileSpec,
	}})
	service, err := NewService(ctx, pool, kube, func(_ context.Context, _ ProjectScope, id string) (DevelopmentTarget, error) {
		return DevelopmentTarget{
			Namespace: namespace, ApprovedProfiles: []string{"arm"},
			EchoPackage: "registry.example.test/echo@sha256:" + strings.Repeat("2", 64),
			Trigger: platform.HTTPTrigger{
				Hostname: id + ".workers.example.test",
				Gateway:  platform.GatewayRef{ObjectName: platform.ObjectName{Name: "dev", Namespace: namespace}, SectionName: "https"},
				Router:   platform.RouterRef{ObjectName: platform.ObjectName{Name: "router", Namespace: "ate"}, Port: 443},
			},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanup, "DELETE FROM worker_apps WHERE organization_id = $1", organization); err != nil {
			t.Error("테스트 등록 데이터 정리 실패")
		}
	})
	handler := Handler(func(r *http.Request, project string, write bool) (ProjectScope, error) {
		switch r.Header.Get("X-Test-Role") {
		case "read":
			if write {
				return ProjectScope{}, ErrForbidden
			}
		case "write":
		default:
			return ProjectScope{}, ErrUnauthorized
		}
		return ProjectScope{OrganizationID: organization, ProjectID: project}, nil
	}, service)
	request := func(method, path, role, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Test-Role", role)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	check := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP %d를 예상했습니다. HTTP %d: %s", status, w.Code, w.Body)
		}
	}
	const collection = "/projects/first/worker-apps"
	const registration = `{"name":"Echo","profile":"arm","package":{"sample":"echo"}}`
	check(request("POST", collection, "read", registration), http.StatusForbidden)
	check(request("GET", collection, "", ""), http.StatusUnauthorized)
	check(request("POST", collection, "write", strings.Replace(registration, "\"arm\"", "\"unapproved\"", 1)), http.StatusBadRequest)
	registrations := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { registrations <- request("POST", collection, "write", registration) }()
	}
	var id string
	for range 2 {
		response := <-registrations
		check(response, http.StatusCreated)
		var app App
		if err := json.Unmarshal(response.Body.Bytes(), &app); err != nil || app.ID == "" || id != "" && app.ID != id {
			t.Fatalf("동시 등록에서 동일한 앱 ID를 예상했습니다: %+v, %v", app, err)
		}
		id = app.ID
	}
	path := collection + "/" + id + "/environments/development"
	check(request("PUT", path, "read", "{}"), http.StatusForbidden)
	check(request("GET", strings.Replace(path, "/first/", "/second/", 1), "read", ""), http.StatusNotFound)
	deployments := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { deployments <- request("PUT", path, "write", "{}") }()
	}
	for range 2 {
		check(<-deployments, http.StatusAccepted)
	}
	worker, _, err := deploymentIdentity(organization, "first", id)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := kube.Resource(workerResource).Namespace(namespace).Get(ctx, worker, metav1.GetOptions{})
	if err != nil || resource.GetName() != worker {
		t.Fatalf("배포 HTTPWorker 조회 실패: %v", err)
	}
	response := request("GET", path, "read", "")
	check(response, http.StatusOK)
	var status DevelopmentStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || status.LatestAttempt.State != "queued" || status.Active != nil {
		t.Fatalf("최초 배포 상태가 예상과 다릅니다: %+v, %v", status, err)
	}
}
