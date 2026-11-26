package workerapps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

var (
	ErrDeploymentNotReady = errors.New("활성 개발 배포를 먼저 준비하세요")
	ErrInvocation         = errors.New("워커 앱에서 JSON 응답을 받지 못했습니다")
)

const invocationLimit = 64 << 10

type InvocationResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

var invocationClient = &http.Client{
	Timeout: 8 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (s *Service) Invoke(ctx context.Context, scope ProjectScope, id string, input json.RawMessage) (InvocationResult, error) {
	if _, err := s.app(ctx, scope, id); err != nil {
		return InvocationResult{}, err
	}
	target, err := s.target(ctx, scope, id)
	if err != nil {
		return InvocationResult{}, err
	}
	status, err := target.ReadDevelopment(ctx, s.kube, scope.OrganizationID, scope.ProjectID, id)
	if err != nil {
		return InvocationResult{}, err
	}
	if status.Active == nil || status.LatestAttempt.Reason == "RouteRecoveryUnconfirmed" {
		return InvocationResult{}, ErrDeploymentNotReady
	}

	// 목적지는 프로젝트의 서버 설정에서만 선택한다.
	destination := url.URL{Scheme: "https", Host: target.Trigger.Hostname, Path: status.Operation.Path}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, destination.String(), bytes.NewReader(input))
	if err != nil {
		return InvocationResult{}, ErrInvocation
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := invocationClient.Do(request)
	if err != nil {
		return InvocationResult{}, ErrInvocation
	}
	defer response.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(response.Body, invocationLimit+1))
	if err != nil || len(reply) > invocationLimit || !json.Valid(reply) {
		return InvocationResult{}, ErrInvocation
	}
	return InvocationResult{Status: response.StatusCode, Body: reply}, nil
}
