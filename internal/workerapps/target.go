package workerapps

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// LinkTarget은 API 연산이 추종할 앱의 개발 대상이다.
type LinkTarget struct {
	AppID       string        `json:"appId"`
	ProjectID   string        `json:"projectId"`
	Environment string        `json:"environment"`
	Operation   HTTPOperation `json:"operation"`
	URL         string        `json:"url,omitempty"`
	Revision    string        `json:"revision,omitempty"`
}

func (s *Service) LinkTarget(ctx context.Context, scope ProjectScope, id string) (LinkTarget, error) {
	if _, err := s.app(ctx, scope, id); err != nil {
		return LinkTarget{}, err
	}
	target, err := s.target(ctx, scope, id)
	if err != nil {
		return LinkTarget{}, err
	}
	link := LinkTarget{
		AppID: id, ProjectID: scope.ProjectID, Environment: "development",
		Operation: HTTPOperation{Method: http.MethodPost, Path: "/invoke"},
	}
	status, err := target.ReadDevelopment(ctx, s.kube, scope.OrganizationID, scope.ProjectID, id)
	if errors.Is(err, ErrDeploymentNotFound) {
		return link, nil
	}
	if err != nil {
		return LinkTarget{}, err
	}
	if status.Active != nil {
		endpoint := url.URL{Scheme: "https", Host: target.Trigger.Hostname, Path: link.Operation.Path}
		link.URL = endpoint.String()
		link.Revision = status.Active.Revision
	}
	return link, nil
}
