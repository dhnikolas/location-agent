// Package runtime reconciles Runtime resources onto local containers.
package runtime

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/reconcile-kit/api/conditions"
	"github.com/reconcile-kit/api/resource"
	cl "github.com/reconcile-kit/controlloop"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
	runtimesvc "scm.x5.ru/dis.cloud/core/location-agent/internal/services/runtime"
)

const (
	RuntimeFinalizer = "runtime.agent-platform.salt.x5.ru/location-agent"

	requeueInterval = 10 * time.Second
)

// StorageBinder receives the storage set once reconcile-kit provides it.
type StorageBinder interface {
	Bind(*cl.StorageSet)
}

type Reconciler[T resource.Object[T]] struct {
	storage   *cl.StorageSet
	logger    *slog.Logger
	svc       *runtimesvc.Service
	resources StorageBinder
}

func NewReconciler[T resource.Object[T]](logger *slog.Logger, svc *runtimesvc.Service, resources StorageBinder) *Reconciler[T] {
	return &Reconciler[T]{logger: logger, svc: svc, resources: resources}
}

func (r *Reconciler[T]) SetStorage(storage *cl.StorageSet) {
	r.storage = storage
	r.resources.Bind(storage)
}

func (r *Reconciler[T]) Reconcile(ctx context.Context, object *platform.Runtime) (result cl.Result, reterr error) {
	client, ok := cl.GetStorage[*platform.Runtime](r.storage)
	if !ok {
		return cl.Result{}, nil
	}

	oldObject := object.DeepCopy()
	defer func() {
		object.Status.ObservedGeneration = object.Version
		empty := cl.Result{}
		if result == empty && reterr == nil {
			object.SetCurrentVersion(object.GetVersion())
		}
		conditions.SyncReady(object)
		if equalStatus(oldObject, object) {
			return
		}
		if err := client.UpdateStatus(ctx, object); err != nil {
			reterr = err
			r.logger.Error("update runtime status",
				"namespace", object.Namespace, "name", object.Name, "error", err)
		}
	}()

	if !slices.Contains(object.Finalizers, RuntimeFinalizer) && object.DeletionTimestamp == "" {
		object.AddFinalizer(RuntimeFinalizer)
		return cl.Result{Requeue: true}, nil
	}

	if object.DeletionTimestamp != "" {
		return r.reconcileDelete(ctx, object)
	}
	return r.reconcileNormal(ctx, object)
}

func (r *Reconciler[T]) reconcileNormal(ctx context.Context, object *platform.Runtime) (cl.Result, error) {
	state, err := r.svc.Ensure(ctx, object)
	if err != nil {
		// Logged as well as recorded: a condition is only visible to whoever
		// thinks to look at the resource, and this runs on someone's laptop.
		r.logger.Error("ensure runtime", "runtime", object.Name, "error", err)
		object.Status.Phase = platform.PhaseFailed
		conditions.MarkFalse(object, platform.RuntimeCond, "Ensure", err.Error())
		return cl.Result{RequeueAfter: requeueInterval}, nil
	}

	object.Status.TCP = state.TCP
	object.Status.Ingress = state.Ingress

	if !state.Running {
		object.Status.Phase = platform.PhaseInitializing
		conditions.MarkFalse(object, platform.RuntimeCond, "Starting", "container is not running yet")
		return cl.Result{RequeueAfter: requeueInterval}, nil
	}

	object.Status.Phase = platform.PhaseReady
	conditions.MarkTrue(object, platform.RuntimeCond)
	// Re-checked on a slow tick: a container can die without anything changing
	// in the control plane, and nothing else would notice.
	return cl.Result{RequeueAfter: requeueInterval}, nil
}

func (r *Reconciler[T]) reconcileDelete(ctx context.Context, object *platform.Runtime) (cl.Result, error) {
	object.Status.Phase = platform.PhaseTerminating

	if err := r.svc.Remove(ctx, object); err != nil {
		conditions.MarkFalse(object, platform.RuntimeCond, "Remove", err.Error())
		return cl.Result{RequeueAfter: requeueInterval}, nil
	}

	object.RemoveFinalizer(RuntimeFinalizer)
	return cl.Result{}, nil
}

// equalStatus reports whether anything worth writing back changed. Runtimes
// are re-checked on a timer, so without this every tick would be a write.
func equalStatus(a, b *platform.Runtime) bool {
	if a.Status.Phase != b.Status.Phase ||
		a.Status.ObservedGeneration != b.Status.ObservedGeneration ||
		len(a.Status.Conditions) != len(b.Status.Conditions) ||
		len(a.Finalizers) != len(b.Finalizers) {
		return false
	}
	for i := range a.Status.Conditions {
		if a.Status.Conditions[i] != b.Status.Conditions[i] {
			return false
		}
	}
	return slices.Equal(a.Status.TCP, b.Status.TCP) &&
		slices.Equal(a.Status.Ingress, b.Status.Ingress)
}
