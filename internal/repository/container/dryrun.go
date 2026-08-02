// Package container implements the machine's container engine.
package container

import (
	"context"
	"log/slog"

	runtimesvc "scm.x5.ru/dis.cloud/core/location-agent/internal/services/runtime"
)

// DryRun reports that nothing exists and creates nothing, logging what it
// would have done. It exists so the control-plane half can be exercised on a
// machine — or in a state — where actually starting containers is not wanted.
type DryRun struct{ log *slog.Logger }

func NewDryRun(log *slog.Logger) *DryRun { return &DryRun{log: log} }

func (d *DryRun) Find(context.Context, string) (runtimesvc.Observed, bool, error) {
	return runtimesvc.Observed{}, false, nil
}

func (d *DryRun) List(context.Context) ([]runtimesvc.Observed, error) { return nil, nil }

func (d *DryRun) EnsureVolume(_ context.Context, runtime, name string) error {
	d.log.Info("dry run: would create volume", "runtime", runtime, "volume", name)
	return nil
}

func (d *DryRun) Create(_ context.Context, c runtimesvc.Container) (runtimesvc.Observed, error) {
	ports := map[string]int{}
	for _, p := range c.Ports {
		ports[p.Name] = p.Host
	}
	d.log.Info("dry run: would create container",
		"name", c.Name, "image", c.Image, "workdir", c.Workdir,
		"ports", ports, "mounts", len(c.Mounts), "env", len(c.Env),
		"hash", c.Labels[runtimesvc.LabelSpecHash])
	return runtimesvc.Observed{
		Name: c.Name, Image: c.Image, State: "dry-run", Running: true,
		Labels: c.Labels, Ports: ports,
	}, nil
}

func (d *DryRun) Start(_ context.Context, name string) error {
	d.log.Info("dry run: would start container", "name", name)
	return nil
}

func (d *DryRun) Remove(_ context.Context, name string, purgeVolumes bool) error {
	d.log.Info("dry run: would remove container", "name", name, "purgeVolumes", purgeVolumes)
	return nil
}
