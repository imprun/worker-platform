package workerapps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/imprun/worker-platform/internal/platform"
	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	ErrAppInUse              = errors.New("영향받는 API의 워커 연결을 해제하고 개발 Gateway에 다시 배포하거나 기존 배포를 내리세요")
	ErrDeletionPending       = errors.New("앱 삭제를 처리 중입니다. 최신 앱 상태를 확인하세요")
	ErrRetirementUnavailable = errors.New("API Platform의 삭제 예약을 확인할 수 없습니다. 같은 앱에서 다시 시도하세요")
)

const appDeletionForUpdate = `SELECT deletion_request, deleted_at IS NOT NULL FROM worker_apps
	WHERE organization_id = $1 AND project_id = $2 AND app_id = $3 FOR UPDATE`

func (s *Service) Delete(ctx context.Context, scope ProjectScope, id string, authorize func(string) error) (bool, error) {
	var requestID string
	var removed bool
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, appDeletionForUpdate, scope.OrganizationID, scope.ProjectID, id).Scan(&requestID, &removed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDeploymentNotFound
		}
		if err != nil || removed || requestID != "" {
			return err
		}

		var request [16]byte
		if _, err := rand.Read(request[:]); err != nil {
			return err
		}
		requestID = hex.EncodeToString(request[:])
		_, err = tx.Exec(ctx, `UPDATE worker_apps SET deletion_request = $4
			WHERE organization_id = $1 AND project_id = $2 AND app_id = $3`,
			scope.OrganizationID, scope.ProjectID, id, requestID)
		return err
	})
	if err != nil || removed {
		return removed, err
	}

	// 권한 서버가 워커의 요청 상태를 조회할 수 있도록 트랜잭션 밖에서 승인받는다.
	if err := authorize(requestID); err != nil {
		if errors.Is(err, ErrAppInUse) {
			// 권한 서버에 저장된 거부 결과만 준비 중인 요청을 되돌릴 수 있다.
			_, resetErr := s.db.Exec(ctx, `UPDATE worker_apps SET deletion_request = ''
				WHERE organization_id = $1 AND project_id = $2 AND app_id = $3 AND deletion_request = $4 AND deleted_at IS NULL`,
				scope.OrganizationID, scope.ProjectID, id, requestID)
			if resetErr != nil {
				return false, resetErr
			}
		}
		return false, err
	}

	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var current string
		err := tx.QueryRow(ctx, appDeletionForUpdate, scope.OrganizationID, scope.ProjectID, id).Scan(&current, &removed)
		if err != nil || removed {
			return err
		}
		if current != requestID {
			return ErrDeletionPending
		}
		target, err := s.target(ctx, scope, id)
		if err != nil {
			return err
		}
		removed, err = target.removeDevelopment(ctx, s.kube, scope, id)
		if err != nil || !removed {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE worker_apps SET deleted_at = CURRENT_TIMESTAMP, deletion_request = ''
			WHERE organization_id = $1 AND project_id = $2 AND app_id = $3`,
			scope.OrganizationID, scope.ProjectID, id)
		return err
	})
	return removed && err == nil, err
}

func (target DevelopmentTarget) removeDevelopment(ctx context.Context, client dynamic.Interface, scope ProjectScope, id string) (bool, error) {
	name, owner, err := deploymentIdentity(scope.OrganizationID, scope.ProjectID, id)
	if err != nil {
		return false, err
	}
	workers := client.Resource(workerResource).Namespace(target.Namespace)
	worker, err := workers.Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		uid := worker.GetUID()
		if uid == "" || worker.GetAnnotations()[appOwnerAnnotation] != owner {
			return false, ErrDeploymentChanged
		}
		if worker.GetDeletionTimestamp() != nil {
			return false, nil
		}
		foreground := metav1.DeletePropagationForeground
		if err := workers.Delete(ctx, name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &foreground,
		}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		return false, nil
	case !apierrors.IsNotFound(err):
		return false, err
	}

	// 소유한 HTTPWorker의 종속 HTTPRoute가 없어져야 등록 삭제를 완료한다.
	routes := schema.GroupVersionResource{Group: platform.GatewayAPI, Version: "v1", Resource: "httproutes"}
	_, err = client.Resource(routes).Namespace(target.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}
