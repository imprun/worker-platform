package workerapps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

var (
	ErrUnauthorized = errors.New("로그인이 필요합니다")
	ErrForbidden    = errors.New("이 프로젝트에 대한 권한이 없습니다")
	ErrInvalidInput = errors.New("앱 이름, 승인된 프로필과 앱 패키지를 확인하세요")
)

type ProjectScope struct {
	OrganizationID string
	ProjectID      string
}

// ProjectAccess는 인증 정보를 확인하고 서버가 저장한 조직 및 프로젝트 ID를 반환한다.
type ProjectAccess func(r *http.Request, project string, write bool) (ProjectScope, error)

type Registration struct {
	Name    string        `json:"name"`
	Profile string        `json:"profile"`
	Package PackageChoice `json:"package"`
}

type App struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Registration
}

type DeployRequest struct {
	Retry           bool           `json:"retry,omitempty"`
	ResourceVersion string         `json:"resourceVersion,omitempty"`
	Package         *PackageChoice `json:"package,omitempty"`
}

type AppService interface {
	Register(context.Context, ProjectScope, Registration) (App, error)
	List(context.Context, ProjectScope) ([]App, error)
	Status(context.Context, ProjectScope, string) (DevelopmentStatus, error)
	Deploy(context.Context, ProjectScope, string, DeployRequest) (DevelopmentStatus, error)
}

func Handler(access ProjectAccess, service AppService) http.Handler {
	mux := http.NewServeMux()
	const collection = "/projects/{project}/worker-apps"
	const development = collection + "/{app}/environments/development"
	mux.HandleFunc("POST "+collection, scoped(access, true, func(w http.ResponseWriter, r *http.Request, scope ProjectScope) error {
		var request Registration
		if err := decodeRequest(w, r, &request); err != nil {
			return err
		}
		app, err := service.Register(r.Context(), scope, request)
		return respond(w, http.StatusCreated, app, err)
	}))
	mux.HandleFunc("GET "+collection, scoped(access, false, func(w http.ResponseWriter, r *http.Request, scope ProjectScope) error {
		apps, err := service.List(r.Context(), scope)
		if apps == nil {
			apps = []App{}
		}
		return respond(w, http.StatusOK, apps, err)
	}))
	mux.HandleFunc("GET "+development, scoped(access, false, func(w http.ResponseWriter, r *http.Request, scope ProjectScope) error {
		status, err := service.Status(r.Context(), scope, r.PathValue("app"))
		return respond(w, http.StatusOK, status, err)
	}))
	mux.HandleFunc("PUT "+development, scoped(access, true, func(w http.ResponseWriter, r *http.Request, scope ProjectScope) error {
		var request DeployRequest
		if err := decodeRequest(w, r, &request); err != nil {
			return err
		}
		status, err := service.Deploy(r.Context(), scope, r.PathValue("app"), request)
		return respond(w, http.StatusAccepted, status, err)
	}))
	return mux
}

func scoped(access ProjectAccess, write bool, handle func(http.ResponseWriter, *http.Request, ProjectScope) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := access(r, r.PathValue("project"), write)
		if err == nil {
			if scope.OrganizationID == "" || scope.ProjectID == "" {
				err = ErrForbidden
			} else {
				err = handle(w, r, scope)
			}
		}
		if err == nil {
			return
		}
		code, message := http.StatusInternalServerError, "워커 앱 요청을 처리하지 못했습니다"
		for _, expected := range []struct {
			err  error
			code int
		}{
			{ErrUnauthorized, http.StatusUnauthorized},
			{ErrForbidden, http.StatusForbidden},
			{ErrDeploymentNotFound, http.StatusNotFound},
			{ErrDeploymentChanged, http.StatusConflict},
			{ErrInvalidInput, http.StatusBadRequest},
		} {
			if errors.Is(err, expected.err) {
				code, message = expected.code, expected.err.Error()
				break
			}
		}
		_ = respond(w, code, map[string]string{"error": message}, nil)
	}
}

func decodeRequest(w http.ResponseWriter, r *http.Request, into any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return ErrInvalidInput
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ErrInvalidInput
	}
	return nil
}

func respond(w http.ResponseWriter, code int, value any, err error) error {
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
	return nil
}
