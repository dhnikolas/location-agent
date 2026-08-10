// Package storage reads platform resources from the reconcile-kit storage set.
package storage

import (
	"context"
	"fmt"
	"strings"

	"github.com/reconcile-kit/api/resource"
	cl "github.com/reconcile-kit/controlloop"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
	agentapi "scm.x5.ru/dis.cloud/core/location-agent/api"
	pa "scm.x5.ru/dis.cloud/core/provision-agent/api"
	"scm.x5.ru/dis.cloud/core/provision-agent/pkg/userconfig"
)

// Set reads resources the agent needs but does not own. The underlying storage
// set is handed over by reconcile-kit at start-up, so it is bound later.
type Set struct {
	set   *cl.StorageSet
	shard string
	// local is the profile this machine keeps for the boxes it runs, mixed into
	// the platform's on the way out. Nil when the machine keeps none.
	local *pa.UserConfig
}

// New takes the location this agent serves: listing is shard-wide, and without
// it a machine would plan its containers from every other machine's resources
// too. The local profile is what this machine adds to every profile it reads.
func New(shard string, local *pa.UserConfig) *Set { return &Set{shard: shard, local: local} }

func (s *Set) Bind(set *cl.StorageSet) { s.set = set }

// RuntimeTemplate fetches a template from the shared catalog.
func (s *Set) RuntimeTemplate(ctx context.Context, name string) (*platform.RuntimeTemplate, bool, error) {
	if name == "" {
		return nil, false, nil
	}
	client, ok := cl.GetStorage[*platform.RuntimeTemplate](s.set)
	if !ok {
		return nil, false, fmt.Errorf("runtime template storage is not registered")
	}
	key := resource.ObjectKey{Namespace: platform.NamespaceCatalog, Name: name}
	tpl, exist, err := client.Get(ctx, key)
	if err != nil {
		return nil, false, fmt.Errorf("get runtime template %q: %w", name, err)
	}
	return tpl, exist, nil
}

// UserConfig fetches a user config. An empty name means the runtime does not
// reference one, which is allowed.
func (s *Set) UserConfig(ctx context.Context, name string) (*pa.UserConfig, bool, error) {
	if name == "" {
		return nil, false, nil
	}
	client, ok := cl.GetStorage[*pa.UserConfig](s.set)
	if !ok {
		return nil, false, fmt.Errorf("user config storage is not registered")
	}
	key := resource.ObjectKey{Namespace: pa.NamespaceUserConfigs, Name: name}
	uc, exist, err := client.Get(ctx, key)
	if err != nil {
		return nil, false, fmt.Errorf("get user config %q: %w", name, err)
	}
	if !exist {
		return nil, false, nil
	}
	// The platform's profile is what a box has; this machine's only adds to it.
	return userconfig.Merge(uc, s.local), true, nil
}

// VolumeMounts lists the host directories asked for a runtime.
//
// Listed rather than watched-and-cached: this runs once per reconcile of one
// runtime, on a machine hosting a handful of them, and a list that is fetched
// cannot go stale between a resource being created and someone wondering why
// nothing happened.
//
// Filtered by the runtime here rather than by a label selector, because the
// reference is already in the spec: a label would be a second copy of it, and
// the two would disagree the first time someone edited one.
func (s *Set) VolumeMounts(ctx context.Context, namespace, runtime string) ([]*agentapi.VolumeMount, error) {
	client, ok := cl.GetStorage[*agentapi.VolumeMount](s.set)
	if !ok {
		return nil, fmt.Errorf("volume mount storage is not registered")
	}
	all, err := client.List(ctx, resource.ListOpts{ShardID: s.shard, Namespace: namespace})
	if err != nil {
		return nil, fmt.Errorf("list volume mounts: %w", err)
	}

	out := make([]*agentapi.VolumeMount, 0, len(all))
	for _, vm := range all {
		if vm.Spec.RuntimeRef.Name != runtime {
			continue
		}
		// One being deleted is already not wanted. Waiting for it to disappear
		// would leave the directory mounted for as long as anything delayed the
		// delete.
		if vm.DeletionTimestamp != "" {
			continue
		}
		out = append(out, vm)
	}
	return out, nil
}

// Provision fetches a runtime's Provision. It lives in the runtime's own
// shard, so the key is built from the runtime name rather than from ours.
func (s *Set) Provision(ctx context.Context, runtime string) (*pa.Provision, bool, error) {
	client, ok := cl.GetStorage[*pa.Provision](s.set)
	if !ok {
		return nil, false, fmt.Errorf("provision storage is not registered")
	}
	key := resource.ObjectKey{Namespace: runtime, Name: runtime}
	p, exist, err := client.Get(ctx, key)
	if err != nil {
		return nil, false, fmt.Errorf("get provision %q: %w", runtime, err)
	}
	return p, exist, nil
}

func (s *Set) CreateProvision(ctx context.Context, p *pa.Provision) error {
	client, ok := cl.GetStorage[*pa.Provision](s.set)
	if !ok {
		return fmt.Errorf("provision storage is not registered")
	}
	return client.Create(ctx, p)
}

func (s *Set) UpdateProvision(ctx context.Context, p *pa.Provision) error {
	client, ok := cl.GetStorage[*pa.Provision](s.set)
	if !ok {
		return fmt.Errorf("provision storage is not registered")
	}
	return client.Update(ctx, p)
}

// DeleteProvision removes a runtime's Provision. An absent one is not an
// error: the caller wanted it gone.
func (s *Set) DeleteProvision(ctx context.Context, runtime string) error {
	client, ok := cl.GetStorage[*pa.Provision](s.set)
	if !ok {
		return fmt.Errorf("provision storage is not registered")
	}
	err := client.Delete(ctx, resource.ObjectKey{Namespace: runtime, Name: runtime})
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "not found") {
		return nil
	}
	return err
}
