package workerapps

import (
	"context"
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/imprun/worker-platform/internal/platform"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
)

//go:embed schema.sql
var schemaSQL string

// TargetResolver는 서버 설정에서 앱별 개발 호스트와 프로젝트의 승인 프로필을 결정한다.
type TargetResolver func(context.Context, ProjectScope, string) (DevelopmentTarget, error)

type Service struct {
	db     *pgxpool.Pool
	kube   dynamic.Interface
	target TargetResolver
}

func NewService(ctx context.Context, db *pgxpool.Pool, kube dynamic.Interface, target TargetResolver) (*Service, error) {
	if db == nil || kube == nil || target == nil {
		return nil, errors.New("앱 저장소, Kubernetes 클라이언트와 개발 환경 설정이 필요합니다")
	}
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(489311756452904614)"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, schemaSQL)
		return err
	}); err != nil {
		return nil, err
	}
	return &Service{db: db, kube: kube, target: target}, nil
}

func (s *Service) Register(ctx context.Context, scope ProjectScope, input Registration) (App, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 || input.Profile == "" {
		return App{}, ErrInvalidInput
	}
	id, _, err := deploymentIdentity(scope.OrganizationID, scope.ProjectID, input.Name)
	if err != nil {
		return App{}, err
	}
	var retired bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM worker_apps WHERE organization_id = $1 AND project_id = $2 AND app_id = $3 AND deleted_at IS NOT NULL
	)`, scope.OrganizationID, scope.ProjectID, id).Scan(&retired); err != nil {
		return App{}, err
	}
	if retired {
		var generation [18]byte
		if _, err := rand.Read(generation[:]); err != nil {
			return App{}, err
		}
		id = fmt.Sprintf("app-%x", generation[:])
	}
	target, profile, err := s.resolveProfile(ctx, scope, id, input.Profile)
	if err != nil {
		return App{}, err
	}
	if _, err := target.Deployment(id, profile, input.Package); err != nil {
		return App{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	// PostgreSQL의 고유 제약으로 다른 서버에서 동시에 요청해도 앱은 한 건만 등록된다.
	_, err = s.db.Exec(ctx, `INSERT INTO worker_apps
		(organization_id, project_id, app_id, name, profile, package)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
		scope.OrganizationID, scope.ProjectID, id, input.Name, input.Profile, input.Package)
	if err != nil {
		return App{}, err
	}
	app, err := scanApp(s.db.QueryRow(ctx, `SELECT `+appColumns+`
		FROM worker_apps WHERE organization_id = $1 AND project_id = $2 AND name = $3 AND deleted_at IS NULL`,
		scope.OrganizationID, scope.ProjectID, input.Name))
	if errors.Is(err, pgx.ErrNoRows) || err == nil && app.Registration != input {
		return App{}, ErrDeploymentChanged
	}
	if err == nil && app.DeletionRequested {
		return App{}, ErrDeletionPending
	}
	return app, err
}

func (s *Service) List(ctx context.Context, scope ProjectScope) ([]App, error) {
	rows, err := s.db.Query(ctx, `SELECT `+appColumns+`
		FROM worker_apps WHERE organization_id = $1 AND project_id = $2 AND deleted_at IS NULL ORDER BY name`,
		scope.OrganizationID, scope.ProjectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (App, error) { return scanApp(row) })
}

func (s *Service) Status(ctx context.Context, scope ProjectScope, id string) (DevelopmentStatus, error) {
	if _, err := s.app(ctx, scope, id); err != nil {
		return DevelopmentStatus{}, err
	}
	target, err := s.target(ctx, scope, id)
	if err != nil {
		return DevelopmentStatus{}, err
	}
	return target.ReadDevelopment(ctx, s.kube, scope.OrganizationID, scope.ProjectID, id)
}

func (s *Service) Deploy(ctx context.Context, scope ProjectScope, id string, request DeployRequest) (DevelopmentStatus, error) {
	var target DevelopmentTarget
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		app, err := scanApp(tx.QueryRow(ctx, appByID+" FOR UPDATE", scope.OrganizationID, scope.ProjectID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDeploymentNotFound
		}
		if err != nil {
			return err
		}
		if app.DeletionRequested {
			return ErrDeletionPending
		}
		resolved, profile, err := s.resolveProfile(ctx, scope, id, app.Profile)
		if err != nil {
			return err
		}
		target = resolved
		choice := app.Package
		if request.Package != nil {
			choice = *request.Package
		}
		_, err = target.SubmitDevelopment(ctx, s.kube, scope.OrganizationID, scope.ProjectID, id, request.ResourceVersion, profile, choice, request.Retry)
		return err
	})
	if err != nil {
		return DevelopmentStatus{}, err
	}
	return target.ReadDevelopment(ctx, s.kube, scope.OrganizationID, scope.ProjectID, id)
}

const appColumns = "app_id, project_id, name, profile, package, deletion_request"
const appByID = `SELECT ` + appColumns + `
	FROM worker_apps WHERE organization_id = $1 AND project_id = $2 AND app_id = $3 AND deleted_at IS NULL`

func (s *Service) app(ctx context.Context, scope ProjectScope, id string) (App, error) {
	app, err := scanApp(s.db.QueryRow(ctx, appByID, scope.OrganizationID, scope.ProjectID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return App{}, ErrDeploymentNotFound
	}
	return app, err
}

func scanApp(row pgx.Row) (App, error) {
	var app App
	err := row.Scan(&app.ID, &app.ProjectID, &app.Name, &app.Profile, &app.Package, &app.deletionRequest)
	app.DeletionRequested = app.deletionRequest != ""
	return app, err
}

func (s *Service) resolveProfile(ctx context.Context, scope ProjectScope, id, name string) (DevelopmentTarget, platform.WorkerProfile, error) {
	target, err := s.target(ctx, scope, id)
	if err != nil {
		return DevelopmentTarget{}, platform.WorkerProfile{}, err
	}
	if target.Namespace == "" {
		return DevelopmentTarget{}, platform.WorkerProfile{}, ErrForbidden
	}
	if !slices.Contains(target.ApprovedProfiles, name) {
		return DevelopmentTarget{}, platform.WorkerProfile{}, ErrInvalidInput
	}
	gvr := workerResource
	gvr.Resource = "workerprofiles"
	resource, err := s.kube.Resource(gvr).Namespace(target.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || err == nil && resource.GetDeletionTimestamp() != nil {
		return DevelopmentTarget{}, platform.WorkerProfile{}, ErrInvalidInput
	}
	if err != nil {
		return DevelopmentTarget{}, platform.WorkerProfile{}, err
	}
	spec, _, err := unstructured.NestedMap(resource.Object, "spec")
	if err != nil {
		return DevelopmentTarget{}, platform.WorkerProfile{}, err
	}
	profile := platform.WorkerProfile{
		APIVersion: platform.WorkerVersion, Kind: "WorkerProfile",
		Metadata: platform.ObjectName{Name: resource.GetName(), Namespace: resource.GetNamespace()},
	}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &profile.Spec)
	return target, profile, err
}
