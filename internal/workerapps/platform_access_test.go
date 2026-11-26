package workerapps

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestPlatformProjectAccessChecksRequestedOrganization(t *testing.T) {
	authority := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v0.9/projects/shared/worker-app-access" ||
			r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Org-Id") != "active-org" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"organizationId":"active-id","projectId":"active-project"}`))
	}))
	t.Cleanup(authority.Close)
	base, err := url.Parse(authority.URL)
	if err != nil {
		t.Fatal(err)
	}
	check := platformProjectAccess(base, authority.Client())
	request := httptest.NewRequest(http.MethodPost, "/projects/shared/worker-apps", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("X-Org-Id", "other-org")
	if _, err := check(request, "shared", true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("권한 서버가 거부한 조직을 사용할 수 있습니다: %v", err)
	}
	request.Header.Set("X-Org-Id", "active-org")
	scope, err := check(request, "shared", true)
	if err != nil || scope != (ProjectScope{OrganizationID: "active-id", ProjectID: "active-project"}) {
		t.Fatalf("권한 서버가 확인한 조직과 프로젝트가 필요합니다: %+v, %v", scope, err)
	}
}
