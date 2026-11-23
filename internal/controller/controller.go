package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/imprun/worker-platform/internal/platform"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	group        = "workerplatform.imprun.dev"
	version      = "v1alpha3"
	profileIndex = "profile"
)

var (
	workersGVR  = schema.GroupVersionResource{Group: group, Version: version, Resource: "httpworkers"}
	profilesGVR = schema.GroupVersionResource{Group: group, Version: version, Resource: "workerprofiles"}
)

type Controller struct {
	workers    dynamic.ResourceInterface
	profiles   dynamic.ResourceInterface
	deployment platform.ATEDeployment
	jobs       JobPublications
}

type JobPublications interface {
	PublishedWorkers(context.Context) ([]string, error)
	PublicationEpoch(context.Context) (int64, error)
	ObserveWorker(ctx context.Context, worker, uid, priorRevision string, enabled bool, epoch int64) (bool, error)
	PublishRevision(ctx context.Context, worker, uid, revision string, epoch int64, snapshot platform.JobRevision) (bool, error)
}

func Run(ctx context.Context, watchClient, client dynamic.Interface, namespace string, deployment platform.ATEDeployment,
	jobs JobPublications) error {
	if namespace == "" {
		return errors.New("controller namespace is required")
	}
	c := Controller{
		workers:    client.Resource(workersGVR).Namespace(namespace),
		profiles:   client.Resource(profilesGVR).Namespace(namespace),
		deployment: deployment,
		jobs:       jobs,
	}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(watchClient, 0, namespace, nil)
	workers := factory.ForResource(workersGVR).Informer()
	if err := workers.AddIndexers(cache.Indexers{profileIndex: func(obj any) ([]string, error) {
		worker := obj.(*unstructured.Unstructured)
		name, _, err := unstructured.NestedString(worker.Object, "spec", "profile")
		return []string{name}, err
	}}); err != nil {
		return err
	}
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, time.Minute),
	)
	defer queue.ShutDown()
	enqueueWorker := func(obj any) {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = tombstone.Obj
		}
		queue.Add(obj.(*unstructured.Unstructured).GetName())
	}
	enqueueProfileWorkers := func(obj any) {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = tombstone.Obj
		}
		profile := obj.(*unstructured.Unstructured)
		matches, err := workers.GetIndexer().ByIndex(profileIndex, profile.GetName())
		if err != nil {
			slog.Error("look up HTTPWorkers by profile", "error", err)
			return
		}
		for _, match := range matches {
			enqueueWorker(match)
		}
	}
	if err := registerEvents(workers, enqueueWorker, enqueueWorker); err != nil {
		return err
	}
	if err := registerEvents(factory.ForResource(profilesGVR).Informer(), enqueueProfileWorkers, enqueueProfileWorkers); err != nil {
		return err
	}
	factory.Start(ctx.Done())
	defer factory.Shutdown()
	if synced := factory.WaitForCacheSync(ctx.Done()); !synced[workersGVR] || !synced[profilesGVR] {
		return ctx.Err()
	}
	if jobs != nil {
		db, cancel := context.WithTimeout(ctx, 8*time.Second)
		names, err := jobs.PublishedWorkers(db)
		cancel()
		if err != nil {
			return fmt.Errorf("list Job publications for reconciliation: %w", err)
		}
		for _, name := range names {
			queue.Add(name)
		}
	}

	var workersDone sync.WaitGroup
	for range 2 {
		workersDone.Go(func() { c.processQueue(ctx, queue) })
	}
	slog.Info("watching HTTPWorkers and WorkerProfiles", "namespace", namespace)
	<-ctx.Done()
	queue.ShutDown()
	workersDone.Wait()
	return nil
}

func registerEvents(informer cache.SharedIndexInformer, enqueue, deleted func(any)) error {
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: enqueue,
		UpdateFunc: func(previous, current any) {
			old, new := previous.(*unstructured.Unstructured), current.(*unstructured.Unstructured)
			if !sameRevision(old, new) || !new.GetDeletionTimestamp().Equal(old.GetDeletionTimestamp()) {
				enqueue(current)
			}
		},
		DeleteFunc: deleted,
	})
	return err
}

func (c Controller) processQueue(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string]) {
	for ctx.Err() == nil {
		name, shutdown := queue.Get()
		if shutdown {
			return
		}
		err := c.reconcile(ctx, name)
		switch {
		case ctx.Err() != nil || err == nil:
			queue.Forget(name)
		case errors.Is(err, errSuperseded):
			queue.Forget(name)
			queue.Add(name)
		default:
			slog.Warn("reconcile HTTPWorker", "name", name, "error", err)
			queue.AddRateLimited(name)
		}
		queue.Done(name)
	}
}

var errSuperseded = errors.New("worker or profile changed during reconciliation")

func (c Controller) reconcile(ctx context.Context, name string) error {
	var epoch int64
	if c.jobs != nil {
		db, cancel := context.WithTimeout(ctx, 8*time.Second)
		var err error
		epoch, err = c.jobs.PublicationEpoch(db)
		cancel()
		if err != nil {
			return fmt.Errorf("begin Job publication reconciliation: %w", err)
		}
	}
	worker, err := c.workers.Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	exists := err == nil && worker.GetDeletionTimestamp() == nil
	if c.jobs != nil {
		var uid, priorRevision string
		var enabled bool
		if exists {
			uid = string(worker.GetUID())
			enabled, _, _ = unstructured.NestedBool(worker.Object, "spec", "jobEnabled")
			priorRevision, _, _ = unstructured.NestedString(worker.Object, "status", "active", "revision")
		}
		db, cancel := context.WithTimeout(ctx, 8*time.Second)
		current, err := c.jobs.ObserveWorker(db, name, uid, priorRevision, enabled, epoch)
		cancel()
		if err != nil || !current {
			return err
		}
	}
	if !exists {
		return nil
	}

	w, err := readWorker(worker)
	if err != nil {
		return c.reject(ctx, worker, nil, "InvalidWorker", err)
	}
	profile, err := c.profiles.Get(ctx, w.Spec.Profile, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || err == nil && profile.GetDeletionTimestamp() != nil {
		return c.reject(ctx, worker, nil, "ProfileMissing", fmt.Errorf("WorkerProfile %s is unavailable", w.Spec.Profile))
	}
	if err != nil {
		return err
	}
	p, err := readProfile(profile)
	if err != nil {
		return c.reject(ctx, worker, profile, "InvalidProfile", err)
	}
	if err := c.record(ctx, worker, profile, "Preparing", "Waiting for Actor readiness and HTTPRoute acceptance", nil); err != nil {
		return err
	}

	d := c.deployment
	d.WorkerOwner = metav1.NewControllerRef(worker, workersGVR.GroupVersion().WithKind("HTTPWorker"))
	// The dependent is in the same namespace and uses background garbage collection.
	d.WorkerOwner.BlockOwnerDeletion = nil
	d.BeforePublish = func(ctx context.Context) error { return c.checkInputs(ctx, worker, profile) }
	attempt, cancel := context.WithTimeout(ctx, 90*time.Second)
	result, err := d.Deploy(attempt, w, p)
	cancel()
	if inputErr := c.checkInputs(ctx, worker, profile); inputErr != nil {
		return inputErr
	}
	if err != nil {
		return errors.Join(err, c.record(ctx, worker, profile, "DeploymentPending", err.Error(), nil))
	}
	if result.Job != nil && c.jobs != nil {
		record, cancel := context.WithTimeout(ctx, 8*time.Second)
		current, err := c.jobs.PublishRevision(record, name, string(worker.GetUID()), result.Revision, epoch, *result.Job)
		cancel()
		if err != nil {
			return fmt.Errorf("publish Job revision: %w", err)
		}
		if !current {
			return nil
		}
	}
	return c.record(ctx, worker, profile, "Published", "Actor ready and HTTPRoute accepted", &result)
}

func (c Controller) reject(ctx context.Context, worker, profile *unstructured.Unstructured, reason string, cause error) error {
	// Spec errors are retried when the worker or its referenced profile changes.
	return c.record(ctx, worker, profile, reason, cause.Error(), nil)
}

func (c Controller) checkInputs(ctx context.Context, worker, profile *unstructured.Unstructured) error {
	if err := checkCurrent(ctx, c.workers, worker); err != nil {
		return err
	}
	return checkCurrent(ctx, c.profiles, profile)
}

func checkCurrent(ctx context.Context, resource dynamic.ResourceInterface, used *unstructured.Unstructured) error {
	current, err := resource.Get(ctx, used.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return errSuperseded
	}
	if err != nil {
		return err
	}
	if !sameRevision(used, current) || current.GetDeletionTimestamp() != nil {
		return errSuperseded
	}
	return nil
}

func sameRevision(a, b *unstructured.Unstructured) bool {
	return a.GetUID() == b.GetUID() && a.GetGeneration() == b.GetGeneration()
}
