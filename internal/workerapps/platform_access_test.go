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

func TestRetirementAccessRequiresStoredDenial(t *testing.T) {
	const appID = "app-0123456789abcdef0123456789abcdef0123"
	const challenge = "0123456789abcdef0123456789abcdef"
	for _, entry := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"승인", http.StatusNoContent, "", nil},
		{"저장한 사용 중 거부", http.StatusConflict, `{"code":"ARTIFACT_DEPLOYED"}`, ErrAppInUse},
		{"판정하지 못한 충돌", http.StatusConflict, `{"code":"COMMON_CONFLICT"}`, ErrRetirementUnavailable},
		{"서비스 연결 오류", http.StatusBadGateway, "", ErrRetirementUnavailable},
	} {
		t.Run(entry.name, func(t *testing.T) {
			authority := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/api/v0.9/projects/shared/worker-apps/"+appID+"/retirement" ||
					r.Header.Get(deletionRequestHeader) != challenge || r.Header.Get("X-Org-Id") != "org-id" ||
					r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("인증한 프로젝트 및 삭제 요청을 권한 서버에 전달해야 합니다")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(entry.status)
				_, _ = w.Write([]byte(entry.body))
			}))
			defer authority.Close()
			base, err := url.Parse(authority.URL)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodDelete, "/projects/shared/worker-apps/"+appID, nil)
			request.SetPathValue("project", "shared")
			request.Header.Set("Authorization", "Bearer test-token")
			check := platformRetirementAccess(base, authority.Client())
			if err := check(request, ProjectScope{OrganizationID: "org-id", ProjectID: "project-id"}, appID, challenge); !errors.Is(err, entry.want) {
				t.Fatalf("삭제 요청 처리 결과가 다릅니다: %v", err)
			}
		})
	}
}
