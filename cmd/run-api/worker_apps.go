package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/imprun/worker-platform/internal/platform"
	"github.com/imprun/worker-platform/internal/workerapps"
	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

type workerAppConfig struct {
	PlatformURL string              `json:"platformURL"`
	Domain      string              `json:"developmentDomain"`
	EchoPackage string              `json:"echoPackage"`
	Gateway     platform.GatewayRef `json:"gateway"`
	Router      platform.RouterRef  `json:"router"`
	Projects    []workerAppProject  `json:"projects"`
}

type workerAppProject struct {
	OrganizationID   string   `json:"organizationId"`
	ProjectID        string   `json:"projectId"`
	ApprovedProfiles []string `json:"approvedProfiles"`
}

// 앱 ID를 개발 호스트의 한 DNS 레이블로 사용한다.
var appHostnameLabel = regexp.MustCompile(`^app-[a-f0-9]{36}$`)

func openWorkerApps(ctx context.Context, configFile, dsnFile, namespace string) (http.Handler, func(), error) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return nil, nil, errors.New("워커 앱 설정을 읽을 수 없습니다")
	}
	var config workerAppConfig
	if err := json.Unmarshal(data, &config); err != nil || config.Domain == "" || len(config.Projects) == 0 {
		return nil, nil, errors.New("워커 앱 개발 도메인과 프로젝트 설정을 확인하세요")
	}
	access, retire, err := workerapps.PlatformProjectAccess(config.PlatformURL)
	if err != nil {
		return nil, nil, err
	}
	projects := make(map[workerapps.ProjectScope][]string, len(config.Projects))
	for _, project := range config.Projects {
		key := workerapps.ProjectScope{OrganizationID: project.OrganizationID, ProjectID: project.ProjectID}
		if project.OrganizationID == "" || project.ProjectID == "" || len(project.ApprovedProfiles) == 0 || projects[key] != nil {
			return nil, nil, errors.New("워커 앱 프로젝트 설정이 누락되었거나 중복되었습니다")
		}
		projects[key] = project.ApprovedProfiles
	}
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, errors.New("워커 앱 Kubernetes 서비스 계정을 구성할 수 없습니다")
	}
	kube, err := dynamic.NewForConfig(kubeConfig)
	if err != nil {
		return nil, nil, err
	}
	dsn, err := os.ReadFile(dsnFile)
	if err != nil {
		return nil, nil, errors.New("워커 앱 PostgreSQL 연결 파일을 읽을 수 없습니다")
	}
	pool, err := pgxpool.New(ctx, strings.TrimSpace(string(dsn)))
	if err != nil {
		return nil, nil, errors.New("워커 앱 PostgreSQL 연결을 구성할 수 없습니다")
	}
	service, err := workerapps.NewService(ctx, pool, kube, func(_ context.Context, scope workerapps.ProjectScope, id string) (workerapps.DevelopmentTarget, error) {
		profiles, ok := projects[scope]
		if !ok {
			return workerapps.DevelopmentTarget{}, workerapps.ErrForbidden
		}
		if !appHostnameLabel.MatchString(id) {
			return workerapps.DevelopmentTarget{}, workerapps.ErrInvalidInput
		}
		return workerapps.DevelopmentTarget{
			Namespace: namespace, ApprovedProfiles: profiles, EchoPackage: config.EchoPackage,
			Trigger: platform.HTTPTrigger{
				Hostname: id + "." + config.Domain,
				Gateway:  config.Gateway, Router: config.Router,
			},
		}, nil
	})
	if err != nil {
		pool.Close()
		return nil, nil, errors.New("워커 앱 PostgreSQL 스키마를 준비할 수 없습니다")
	}
	return workerapps.Handler(func(r *http.Request, project string, write bool) (workerapps.ProjectScope, error) {
		scope, err := access(r, project, write)
		if err == nil && projects[scope] == nil {
			return workerapps.ProjectScope{}, workerapps.ErrForbidden
		}
		return scope, err
	}, service, retire), pool.Close, nil
}
