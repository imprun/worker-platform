package workerapps

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkerAppHTTPDeniesAccessBeforeCallingService(t *testing.T) {
	const base = "/projects/demo/worker-apps"
	for _, entry := range []struct {
		method, path string
		write        bool
	}{
		{http.MethodPost, base, true},
		{http.MethodGet, base, false},
		{http.MethodGet, base + "/app/environments/development", false},
		{http.MethodPut, base + "/app/environments/development", true},
		{http.MethodPost, base + "/app/environments/development/invoke", true},
	} {
		handler := Handler(func(_ *http.Request, project string, write bool) (ProjectScope, error) {
			if project != "demo" || write != entry.write {
				t.Fatalf("요청의 프로젝트나 변경 권한이 다릅니다: %q, %v", project, write)
			}
			return ProjectScope{}, ErrForbidden
		}, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(entry.method, entry.path, strings.NewReader("{}")))
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s %s의 상태가 다릅니다: %d", entry.method, entry.path, response.Code)
		}
	}
}
