package workerapps

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var projectHandle = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// PlatformProjectAccess는 API Platform이 인증한 고정 조직·프로젝트 ID만 사용한다.
func PlatformProjectAccess(endpoint string) (ProjectAccess, error) {
	base, err := url.Parse(endpoint)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("API Platform 권한 서버의 HTTPS 주소가 필요합니다")
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return platformProjectAccess(base, client), nil
}

func platformProjectAccess(base *url.URL, client *http.Client) ProjectAccess {
	return func(r *http.Request, project string, write bool) (ProjectScope, error) {
		authorization := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(authorization, "Bearer ")
		if !ok || token == "" || strings.ContainsAny(token, " \t\r\n") {
			return ProjectScope{}, ErrUnauthorized
		}
		if !projectHandle.MatchString(project) {
			return ProjectScope{}, ErrInvalidInput
		}
		requestedOrganization := r.Header.Get("X-Org-Id")
		if len(r.Header.Values("X-Org-Id")) > 1 ||
			requestedOrganization != "" && !projectHandle.MatchString(requestedOrganization) {
			return ProjectScope{}, ErrForbidden
		}
		method := http.MethodGet
		if write {
			method = http.MethodPost
		}
		target := base.JoinPath("api/v0.9/projects", project, "worker-app-access")
		request, err := http.NewRequestWithContext(r.Context(), method, target.String(), nil)
		if err != nil {
			return ProjectScope{}, err
		}
		request.Header.Set("Authorization", authorization)
		if requestedOrganization != "" {
			request.Header.Set("X-Org-Id", requestedOrganization)
		}
		response, err := client.Do(request)
		if err != nil {
			return ProjectScope{}, err
		}
		defer response.Body.Close()
		switch response.StatusCode {
		case http.StatusUnauthorized:
			return ProjectScope{}, ErrUnauthorized
		case http.StatusForbidden, http.StatusNotFound:
			return ProjectScope{}, ErrForbidden
		case http.StatusOK:
			var scope ProjectScope
			err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&scope)
			return scope, err
		default:
			return ProjectScope{}, errors.New("API Platform 프로젝트 권한을 확인하지 못했습니다")
		}
	}
}
