// Package location keeps the platform's record of this machine current.
package location

import (
	"context"
	"log/slog"
	"maps"
	"sync/atomic"
	"time"

	"github.com/reconcile-kit/api/resource"
	cl "github.com/reconcile-kit/controlloop"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
)

// A location is a laptop, and a laptop is closed, suspended, taken home and
// disconnected from the network. None of that produces an event: the platform
// would go on believing the machine is there, and place runtimes on it that
// nobody starts. So the machine says so itself, repeatedly, and silence is what
// the platform reads as absence.
const (
	// AnnotationHeartbeat is when this agent last reported in, RFC3339.
	AnnotationHeartbeat = platform.LocationGroup + "/heartbeat"
	// AnnotationAgentVersion is the build doing the reporting — useful when a
	// location behaves unlike its neighbours.
	AnnotationAgentVersion = platform.LocationGroup + "/agent-version"

	// Interval is how often the beat is written.
	//
	// Half the window the platform allows, deliberately. With the two equal,
	// every scheduling hiccup on a laptop — and a laptop has many — would show
	// up as a lost connection and clear again a second later, which is worse
	// than no indicator at all.
	Interval = 30 * time.Second
)

type Reconciler[T resource.Object[T]] struct {
	storage *cl.StorageSet
	logger  *slog.Logger
	version string
	// leaving is set once the goodbye has been said. Without it the control
	// loop gets one more turn on the way out — the queue is drained as the
	// process stops — and writes a fresh beat over the removal, leaving the
	// machine looking connected after it is gone.
	leaving atomic.Bool
}

func NewReconciler[T resource.Object[T]](logger *slog.Logger, version string) *Reconciler[T] {
	return &Reconciler[T]{logger: logger, version: version}
}

func (r *Reconciler[T]) SetStorage(storage *cl.StorageSet) { r.storage = storage }

func (r *Reconciler[T]) Reconcile(ctx context.Context, object *platform.Location) (result cl.Result, reterr error) {
	client, ok := cl.GetStorage[*platform.Location](r.storage)
	if !ok {
		return cl.Result{}, nil
	}

	defer func() {
		// The status endpoint, even though what changes is an annotation: it is
		// the write that leaves the spec alone, and a location's spec belongs to
		// whoever configured it, not to this agent. LocationStatus carries no
		// fields of its own, so the annotation is where the beat lives.
		if err := client.UpdateStatus(ctx, object); err != nil {
			reterr = err
			// Logged as well as returned: a location whose beat never lands
			// shows as disconnected in the interface, and the reason for that
			// belongs where a person can find it.
			r.logger.Error("update location status",
				"location", object.Name, "error", err)
		}
	}()

	if object.DeletionTimestamp != "" {
		return r.reconcileDelete(ctx, object)
	}
	return r.reconcileNormal(ctx, object)
}

// reconcileNormal stamps the current time and asks to be called again.
//
// Unconditional: the requeue is the clock, and every run is a beat. There is
// nothing to compare against and no reason to skip a run — a beat written twice
// costs one request, while one skipped by mistake shows the machine as lost.
func (r *Reconciler[T]) reconcileNormal(_ context.Context, object *platform.Location) (cl.Result, error) {
	if r.leaving.Load() {
		return cl.Result{}, nil
	}

	annotations := maps.Clone(object.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[AnnotationHeartbeat] = time.Now().UTC().Format(time.RFC3339)
	if r.version != "" {
		annotations[AnnotationAgentVersion] = r.version
	}
	object.Annotations = annotations
	return cl.Result{RequeueAfter: Interval}, nil
}

// Farewell removes the beat on the way out, so the platform sees the machine as
// off at once instead of waiting out the window.
//
// The annotation is deleted rather than backdated: "last heard from" is a fact
// and should stay one. Its absence is also the more useful signal — a location
// with no beat was switched off on purpose, while one with a stale beat went
// quiet on its own. The two mean different things to whoever is looking, and
// the platform already shows them differently.
//
// Best effort by nature: an agent that is killed outright says nothing, and the
// window is what covers that case.
func (r *Reconciler[T]) Farewell(ctx context.Context, name string) error {
	// Set before the write, not after: whatever the loop does from here on, it
	// must not put the beat back.
	r.leaving.Store(true)

	client, ok := cl.GetStorage[*platform.Location](r.storage)
	if !ok {
		return nil
	}

	object, found, err := client.Get(ctx, resource.ObjectKey{
		Namespace: platform.NamespaceLocations,
		Name:      name,
	})
	if err != nil || !found {
		return err
	}
	if _, beating := object.Annotations[AnnotationHeartbeat]; !beating {
		return nil
	}

	annotations := maps.Clone(object.Annotations)
	delete(annotations, AnnotationHeartbeat)
	// The version stays: it says the machine did run this agent once, which is
	// what tells "switched off" apart from "never connected".
	object.Annotations = annotations

	return client.UpdateStatus(ctx, object)
}

// reconcileDelete stops the beat.
//
// Nothing is cleaned up and no finalizer is held: this agent does not own the
// location, it only reports on it. Holding one would mean a laptop that is
// switched off keeps its location from ever being deleted.
func (r *Reconciler[T]) reconcileDelete(_ context.Context, _ *platform.Location) (cl.Result, error) {
	return cl.Result{}, nil
}
