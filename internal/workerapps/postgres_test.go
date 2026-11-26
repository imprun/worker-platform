package workerapps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imprun/worker-platform/internal/platform"
	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
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
	var identity [16]byte
	rand.Read(identity[:])
	suffix := hex.EncodeToString(identity[:])
	organization := "worker-app-http-test-" + suffix
	namespace := "worker-dev-" + suffix
	profile := platform.WorkerProfileSpec{
		Atespace: namespace, WorkerPool: platform.ObjectName{Name: "arm", Namespace: "ate"},
		PoolID: "arm", Architecture: "arm64", SandboxClass: "gvisor", CPU: "20m", Memory: "64Mi",
		RuntimeImage: "registry.example.test/runtime@sha256:" + strings.Repeat("1", 64),
		Egress:       []platform.EgressRule{{Protocol: "tlsPassthrough", Hostname: "registry.example.test", Port: 443}},
	}
	profileSpec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&profile)
	if err != nil {
		t.Fatal(err)
	}
	kube := workerAppTestKubernetes(t, namespace, &unstructured.Unstructured{Object: map[string]any{
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
	var server *httptest.Server
	var deletionResult atomic.Int32
	var lastDeletionRequest atomic.Value
	lastDeletionRequest.Store("")
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
	}, service, func(r *http.Request, scope ProjectScope, appID, requestID string) error {
		lastDeletionRequest.Store(requestID)
		targetPath := "/projects/" + scope.ProjectID + "/worker-apps/" + appID + "/environments/development/target"
		query, err := http.NewRequestWithContext(r.Context(), http.MethodGet, server.URL+targetPath, nil)
		if err != nil {
			return err
		}
		query.Header.Set("X-Test-Role", "read")
		query.Header.Set(deletionRequestHeader, requestID)
		response, err := server.Client().Do(query)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		var target LinkTarget
		if err := json.NewDecoder(response.Body).Decode(&target); err != nil || response.StatusCode != http.StatusOK ||
			!target.DeletionRequested || !target.DeletionVerified || target.URL != "" {
			t.Error("삭제 예약 서버가 정확한 워커 요청과 대상 상태를 확인할 수 있어야 합니다")
			return ErrRetirementUnavailable
		}
		switch deletionResult.Load() {
		case 1:
			return ErrRetirementUnavailable
		case 2:
			return nil
		default:
			return ErrAppInUse
		}
	})
	server = httptest.NewServer(handler)
	t.Cleanup(server.Close)
	request := func(method, path, role, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		if err == nil {
			r.Header.Set("X-Test-Role", role)
			var response *http.Response
			response, err = server.Client().Do(r)
			if err == nil {
				defer response.Body.Close()
				w.WriteHeader(response.StatusCode)
				_, err = io.Copy(w, response.Body)
			}
		}
		if err != nil {
			t.Error("로컬 워커 앱 HTTP 요청 실패")
		}
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
	targetResponse := request("GET", path+"/target", "read", "")
	check(targetResponse, http.StatusOK)
	var target LinkTarget
	if err := json.Unmarshal(targetResponse.Body.Bytes(), &target); err != nil || target.AppID != id || target.URL != "" {
		t.Fatalf("첫 배포 전 대상 상태가 다릅니다: %+v, %v", target, err)
	}
	check(request("GET", strings.Replace(path, "/first/", "/second/", 1)+"/target", "read", ""), http.StatusNotFound)
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
	generation := resource.GetGeneration()
	profileRef := map[string]any{"name": "arm", "uid": "fixture-profile", "generation": int64(1)}
	resource.Object["status"] = map[string]any{
		"observedGeneration": generation,
		"observedProfile":    profileRef,
		"active": map[string]any{
			"generation": generation, "profile": profileRef, "revision": "v1",
			"atespace": namespace, "actor": "test", "actorUID": "fixture-actor",
			"worker": "test-worker", "workerPod": "test-worker-pod", "route": namespace + "/" + worker,
		},
		"conditions": []any{map[string]any{
			"type": "Ready", "status": "True", "observedGeneration": generation,
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339), "reason": "Published", "message": "테스트",
		}},
	}
	if _, err := kube.Resource(workerResource).Namespace(namespace).UpdateStatus(ctx, resource, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	targetResponse = request("GET", path+"/target", "read", "")
	check(targetResponse, http.StatusOK)
	if err := json.Unmarshal(targetResponse.Body.Bytes(), &target); err != nil ||
		target.URL != "https://"+id+".workers.example.test/invoke" || target.Revision != "v1" {
		t.Fatalf("활성 앱 대상이 예상과 다릅니다: %+v, %v", target, err)
	}
	if resource.GetResourceVersion() != "" {
		initial := request("GET", path, "read", "")
		check(initial, http.StatusOK)
		if err := json.Unmarshal(initial.Body.Bytes(), &status); err != nil || status.Active == nil || status.ResourceVersion == "" {
			t.Fatalf("활성 버전과 리소스 버전이 필요합니다: %+v, %v", status, err)
		}
		originalVersion := status.ResourceVersion
		update := func(digest string) *httptest.ResponseRecorder {
			input, err := json.Marshal(DeployRequest{ResourceVersion: originalVersion, Package: &PackageChoice{
				Repository: "registry.example.test/echo", Digest: digest,
			}})
			if err != nil {
				t.Fatal(err)
			}
			return request("PUT", path, "write", string(input))
		}
		next := "sha256:" + strings.Repeat("3", 64)
		check(update(next), http.StatusAccepted)
		check(update(next), http.StatusAccepted)
		check(update("sha256:"+strings.Repeat("4", 64)), http.StatusConflict)
		latest := request("GET", path, "read", "")
		check(latest, http.StatusOK)
		if err := json.Unmarshal(latest.Body.Bytes(), &status); err != nil || status.Active == nil ||
			status.Active.Revision != "v1" || status.LatestAttempt.State != "queued" ||
			status.LatestAttempt.AppPackage != "registry.example.test/echo@"+next ||
			status.LatestAttempt.Generation <= status.Active.Generation {
			t.Fatalf("v2 준비 중 v1이 보존되어야 합니다: %+v, %v", status, err)
		}
	}

	appPath := collection + "/" + id
	check(request("DELETE", appPath, "read", ""), http.StatusForbidden)
	check(request("DELETE", strings.Replace(appPath, "/first/", "/second/", 1), "write", ""), http.StatusNotFound)
	check(request("DELETE", appPath, "write", ""), http.StatusConflict)
	testTarget := request("GET", path+"/target", "read", "")
	target = LinkTarget{}
	check(testTarget, http.StatusOK)
	if err := json.Unmarshal(testTarget.Body.Bytes(), &target); err != nil || target.DeletionRequested || target.Revision != "v1" {
		t.Fatalf("삭제를 거부한 앱의 활성 배포를 유지해야 합니다: %+v, %v", target, err)
	}
	refused := lastDeletionRequest.Load().(string)

	deletionResult.Store(1)
	check(request("DELETE", appPath, "write", ""), http.StatusServiceUnavailable)
	pending := lastDeletionRequest.Load().(string)
	if pending == "" || pending == refused {
		t.Fatal("거부된 요청을 다시 사용할 수 없습니다")
	}
	check(request("PUT", path, "write", "{}"), http.StatusConflict)
	visible := request("GET", path+"/target", "read", "")
	check(visible, http.StatusOK)
	if strings.Contains(visible.Body.String(), pending) || strings.Contains(visible.Body.String(), "deletionVerified") {
		t.Fatal("요청 식별자나 검증 결과를 일반 대상 조회에 노출할 수 없습니다")
	}

	if os.Getenv("WORKER_APP_TEST_KUBE_FILE") != "" {
		// 임시 API 서버에는 가비지 컬렉션 컨트롤러가 없다.
		return
	}
	workers := kube.Resource(workerResource).Namespace(namespace)
	owned, err := workers.Get(ctx, worker, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owned.SetUID("owned-test-worker")
	if _, err := workers.Update(ctx, owned, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	routeResource := schema.GroupVersionResource{Group: platform.GatewayAPI, Version: "v1", Resource: "httproutes"}
	routes := kube.Resource(routeResource).Namespace(namespace)
	if _, err := routes.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": routeResource.GroupVersion().String(), "kind": "HTTPRoute",
		"metadata": map[string]any{"name": worker, "namespace": namespace},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deletionResult.Store(2)
	check(request("DELETE", appPath, "write", ""), http.StatusAccepted)
	check(request("DELETE", appPath, "write", ""), http.StatusAccepted)
	if lastDeletionRequest.Load().(string) != pending {
		t.Fatal("승인 결과가 불확실하면 같은 삭제 요청을 다시 사용해야 합니다")
	}
	if err := routes.Delete(ctx, worker, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	check(request("DELETE", appPath, "write", ""), http.StatusNoContent)
	check(request("DELETE", appPath, "write", ""), http.StatusNoContent)
	check(request("GET", path+"/target", "read", ""), http.StatusNotFound)
	check(request("PUT", path, "write", "{}"), http.StatusNotFound)
	recreated := request("POST", collection, "write", registration)
	check(recreated, http.StatusCreated)
	var app App
	if err := json.Unmarshal(recreated.Body.Bytes(), &app); err != nil || app.ID == "" || app.ID == id {
		t.Fatalf("같은 이름을 등록해도 삭제된 앱 ID를 재사용할 수 없습니다: %+v, %v", app, err)
	}

}

// workerAppTestKubernetes는 루프백 주소의 전용 API 서버에서만 실제 리소스를 만든다.
func workerAppTestKubernetes(t *testing.T, namespace string, profile *unstructured.Unstructured) dynamic.Interface {
	t.Helper()
	path := os.Getenv("WORKER_APP_TEST_KUBE_FILE")
	if path == "" {
		return fake.NewSimpleDynamicClient(runtime.NewScheme(), profile)
	}
	var settings struct {
		Host     string
		CAData   []byte
		CertData []byte
		KeyData  []byte
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &settings) != nil {
		t.Fatal("격리된 테스트 Kubernetes 연결 설정을 읽을 수 없습니다")
	}
	target, err := url.Parse(settings.Host)
	if err != nil || target.Scheme != "https" || !net.ParseIP(target.Hostname()).IsLoopback() || len(settings.CAData) == 0 {
		t.Fatal("테스트에는 TLS로 검증된 루프백 Kubernetes API가 필요합니다")
	}
	config := &rest.Config{Host: settings.Host, TLSClientConfig: rest.TLSClientConfig{
		CAData: settings.CAData, CertData: settings.CertData, KeyData: settings.KeyData,
	}}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal("테스트 Kubernetes 클라이언트 생성 실패")
	}
	namespaces := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"})
	created, err := namespaces.Create(t.Context(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		uid := created.GetUID()
		if err := namespaces.Delete(ctx, namespace, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
			t.Errorf("테스트 네임스페이스 정리 실패: %v", err)
		}
	})
	profileResource := workerResource
	profileResource.Resource = "workerprofiles"
	if _, err := client.Resource(profileResource).Namespace(namespace).Create(t.Context(), profile, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return client
}
