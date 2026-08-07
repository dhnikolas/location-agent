// Package volumemount reports whether a host directory has reached its box.
//
// It does not mount anything. A bind mount is fixed when a container is
// created, so the only way to add one is to recreate the container — which is
// the runtime controller's job, and doing it from here as well would mean two
// controllers recreating the same container on their own schedules.
//
// So the split is: the runtime reconcile reads these resources and plans the
// container from them, and this reconcile watches the result and says what
// happened. Which also decides how fast a new mount appears — the runtime is
// re-checked on its own tick, so a mount created now is on the box within one
// of those.
package volumemount

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/reconcile-kit/api/conditions"
	"github.com/reconcile-kit/api/resource"
	cl "github.com/reconcile-kit/controlloop"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
	agentapi "scm.x5.ru/dis.cloud/core/location-agent/api"
	runtimesvc "scm.x5.ru/dis.cloud/core/location-agent/internal/services/runtime"
)

// requeueInterval is how often a mount re-checks the box. It matches the
// runtime's own tick: nothing here can change faster than the container it is
// waiting on.
const requeueInterval = 10 * time.Second

// Containers is the machine's container engine, narrowed to the one question
// this controller asks.
type Containers interface {
	Find(ctx context.Context, name string) (runtimesvc.Observed, bool, error)
}

type Reconciler[T resource.Object[T]] struct {
	storage    *cl.StorageSet
	logger     *slog.Logger
	containers Containers
}

func NewReconciler[T resource.Object[T]](logger *slog.Logger, containers Containers) *Reconciler[T] {
	return &Reconciler[T]{logger: logger, containers: containers}
}

func (r *Reconciler[T]) SetStorage(storage *cl.StorageSet) { r.storage = storage }

func (r *Reconciler[T]) Reconcile(ctx context.Context, object *agentapi.VolumeMount) (result cl.Result, reterr error) {
	client, ok := cl.GetStorage[*agentapi.VolumeMount](r.storage)
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
			r.logger.Error("update volume mount status",
				"namespace", object.Namespace, "name", object.Name, "error", err)
		}
	}()

	// No finalizer. Everything this resource caused lives on a container the
	// runtime controller owns: dropping the record is enough for the next
	// runtime reconcile to plan the container without the mount, and holding
	// the record open until then would only delay the delete someone asked for.
	if object.DeletionTimestamp != "" {
		return cl.Result{}, nil
	}

	return r.reconcileNormal(ctx, object)
}

func (r *Reconciler[T]) reconcileNormal(ctx context.Context, object *agentapi.VolumeMount) (cl.Result, error) {
	// The same check the runtime reconcile makes before planning. Said here as
	// well because this is the resource someone reads when the directory does
	// not turn up, and "not reconciled yet" and "that path does not exist" look
	// identical from the box.
	if err := runtimesvc.CheckMount(object.Spec.HostPath, object.Spec.ContainerPath); err != nil {
		object.Status.Phase = platform.PhaseFailed
		conditions.MarkFalse(object, agentapi.VolumeMountCond, "Invalid", err.Error())
		return cl.Result{RequeueAfter: requeueInterval}, nil
	}

	runtimeName := object.Spec.RuntimeRef.Name
	if runtimeName == "" {
		object.Status.Phase = platform.PhaseFailed
		conditions.MarkFalse(object, agentapi.VolumeMountCond, "Invalid", "runtimeRef is empty")
		return cl.Result{RequeueAfter: requeueInterval}, nil
	}

	observed, found, err := r.containers.Find(ctx, runtimeName)
	if err != nil {
		r.logger.Error("inspect runtime container",
			"volumeMount", object.Name, "runtime", runtimeName, "error", err)
		object.Status.Phase = platform.PhaseDegraded
		conditions.MarkFalse(object, agentapi.VolumeMountCond, "Inspect", err.Error())
		return cl.Result{RequeueAfter: requeueInterval}, nil
	}
	if !found {
		object.Status.Phase = platform.PhasePending
		conditions.MarkFalse(object, agentapi.VolumeMountCond, "NoContainer",
			"runtime "+runtimeName+" has no container on this machine yet")
		return cl.Result{RequeueAfter: requeueInterval}, nil
	}

	if !sameHostPath(observed.Binds[object.Spec.ContainerPath], object.Spec.HostPath) {
		// Normal for the moment between this resource appearing and the runtime
		// reconcile recreating the container around it.
		object.Status.Phase = platform.PhaseProvisioning
		conditions.MarkFalse(object, agentapi.VolumeMountCond, "NotMounted",
			"waiting for the runtime container to be recreated with this mount")
		return cl.Result{RequeueAfter: requeueInterval}, nil
	}

	object.Status.Phase = platform.PhaseReady
	conditions.MarkTrue(object, agentapi.VolumeMountCond)
	// Re-checked on a tick: the container can be recreated by anything — a
	// template change, a restart of docker — and nothing would tell us.
	return cl.Result{RequeueAfter: requeueInterval}, nil
}

// sameHostPath compares what docker reports a bind's source to be against what
// was asked for.
//
// Not a plain comparison, because Docker Desktop does not report the path it
// was given: the machine's filesystem reaches the Linux VM through a share, so
// /Users/me/code comes back as /host_mnt/Users/me/code. On Linux, where the
// engine and the machine are the same host, the two are equal.
//
// Getting this wrong is not visible from the container — the mount works either
// way — only from the resource, which would sit at "not mounted yet" forever
// while the directory was plainly there.
func sameHostPath(reported, want string) bool {
	if reported == "" {
		return false
	}
	if reported == want {
		return true
	}
	// Docker Desktop's share, and only as a prefix: a directory genuinely named
	// /host_mnt/... on the machine still compares equal above.
	return strings.TrimPrefix(reported, desktopSharePrefix) == want
}

// desktopSharePrefix is where Docker Desktop's VM sees the machine's files.
const desktopSharePrefix = "/host_mnt"

// equalStatus reports whether anything worth writing back changed. Without it
// every tick would be a write, for a resource whose whole life is spent not
// changing.
func equalStatus(a, b *agentapi.VolumeMount) bool {
	if a.Status.Phase != b.Status.Phase ||
		a.Status.ObservedGeneration != b.Status.ObservedGeneration ||
		len(a.Status.Conditions) != len(b.Status.Conditions) {
		return false
	}
	for i := range a.Status.Conditions {
		if a.Status.Conditions[i] != b.Status.Conditions[i] {
			return false
		}
	}
	return true
}
