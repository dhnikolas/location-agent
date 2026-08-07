package runtime

import (
	"context"
	"io"
	"log/slog"
	"testing"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
	agentapi "scm.x5.ru/dis.cloud/core/location-agent/api"
	pa "scm.x5.ru/dis.cloud/core/provision-agent/api"
)

// fakeContainers records what the service asked the engine to do. Only the
// calls these tests reason about are remembered.
type fakeContainers struct {
	existing  *Observed
	removed   []removal
	volumes   []volumeCall
	created   *Container
	startedAt int
}

type removal struct {
	name  string
	purge bool
}

type volumeCall struct {
	runtime string
	volume  string
}

func (f *fakeContainers) Find(context.Context, string) (Observed, bool, error) {
	if f.existing == nil {
		return Observed{}, false, nil
	}
	return *f.existing, true, nil
}

func (f *fakeContainers) List(context.Context) ([]Observed, error) { return nil, nil }

func (f *fakeContainers) EnsureVolume(_ context.Context, runtime, name string) error {
	f.volumes = append(f.volumes, volumeCall{runtime, name})
	return nil
}

func (f *fakeContainers) Create(_ context.Context, c Container) (Observed, error) {
	f.created = &c
	ports := map[string]int{}
	for _, p := range c.Ports {
		ports[p.Name] = p.Host
	}
	return Observed{Name: c.Name, Labels: c.Labels, Ports: ports, Running: true}, nil
}

func (f *fakeContainers) Start(context.Context, string) error { f.startedAt++; return nil }

func (f *fakeContainers) Remove(_ context.Context, name string, purge bool) error {
	f.removed = append(f.removed, removal{name, purge})
	return nil
}

type fakeResources struct {
	tpl    *platform.RuntimeTemplate
	mounts []*agentapi.VolumeMount
}

func (f fakeResources) RuntimeTemplate(context.Context, string) (*platform.RuntimeTemplate, bool, error) {
	return f.tpl, f.tpl != nil, nil
}

func (f fakeResources) UserConfig(context.Context, string) (*pa.UserConfig, bool, error) {
	return nil, false, nil
}

func (f fakeResources) VolumeMounts(context.Context, string, string) ([]*agentapi.VolumeMount, error) {
	return f.mounts, nil
}

type fakeProvision struct{ removed []string }

func (f *fakeProvision) Ensure(context.Context, string, platform.ProvisionDefaults, *pa.UserConfig) error {
	return nil
}

func (f *fakeProvision) Remove(_ context.Context, runtime string) error {
	f.removed = append(f.removed, runtime)
	return nil
}

func testService(t *testing.T, containers ContainerRepository) (*Service, *fakeProvision) {
	t.Helper()
	claim := &platform.PVCSource{}
	tpl := &platform.RuntimeTemplate{
		Spec: platform.RuntimeTemplateSpec{
			Container: platform.ContainerSpec{
				Image: "claude-box:latest",
				VolumeMounts: []platform.VolumeMount{
					{Name: "home", MountPath: "/home/dev"},
					{Name: "workspace", MountPath: "/work"},
				},
			},
			Volumes: []platform.Volume{
				{Name: "home", PersistentVolumeClaim: claim},
				{Name: "workspace", PersistentVolumeClaim: claim},
			},
		},
	}
	prov := &fakeProvision{}
	svc := New(Config{Location: "nikolai-laptop", BindAddress: "127.0.0.1", PortMin: 31000, PortMax: 31010},
		fakeResources{tpl: tpl}, containers, prov, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, prov
}

func runtimeFor(name string) *platform.Runtime {
	rt := &platform.Runtime{}
	rt.Name = name
	rt.Namespace = name
	rt.Spec.TemplateRef.Name = "claude-box"
	return rt
}

// Deleting a runtime is how a box is disposed of, so its disk goes with it.
// Without this a deleted box keeps its volumes forever, and a runtime later
// created under the same name inherits the old one's work directory and its
// enrolled ssh keys.
func TestRemoveTakesTheVolumes(t *testing.T) {
	containers := &fakeContainers{}
	svc, prov := testService(t, containers)

	if err := svc.Remove(context.Background(), runtimeFor("new-dev")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if len(containers.removed) != 1 {
		t.Fatalf("removals = %v, want exactly one", containers.removed)
	}
	if got := containers.removed[0]; got.name != "new-dev" || !got.purge {
		t.Errorf("removed %+v, want new-dev with the volumes", got)
	}
	if len(prov.removed) != 1 || prov.removed[0] != "new-dev" {
		t.Errorf("provision removals = %v", prov.removed)
	}
}

// A spec change recreates the container, and that path must never touch the
// volumes: it runs on every template edit, and losing the workspace because a
// port moved would be indistinguishable from data loss at random.
func TestRecreateKeepsTheVolumes(t *testing.T) {
	containers := &fakeContainers{
		existing: &Observed{
			Name:   "new-dev",
			Labels: map[string]string{LabelSpecHash: "stale"},
		},
	}
	svc, _ := testService(t, containers)

	if _, err := svc.Ensure(context.Background(), runtimeFor("new-dev")); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	if len(containers.removed) != 1 {
		t.Fatalf("removals = %v, want exactly one", containers.removed)
	}
	if containers.removed[0].purge {
		t.Fatal("recreating the container purged its volumes — the user's work is gone")
	}
}

// The engine needs to know which runtime a volume belongs to, because that is
// what removal matches on later. Matching by name cannot work: "new-dev-home"
// belongs to runtime "new-dev", but nothing in the name rules out a runtime
// "new" with a volume "dev-home".
func TestVolumesAreCreatedUnderTheirRuntime(t *testing.T) {
	containers := &fakeContainers{}
	svc, _ := testService(t, containers)

	if _, err := svc.Ensure(context.Background(), runtimeFor("new-dev")); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	if len(containers.volumes) != 2 {
		t.Fatalf("volume calls = %v, want home and workspace", containers.volumes)
	}
	for _, v := range containers.volumes {
		if v.runtime != "new-dev" {
			t.Errorf("volume %q created under runtime %q", v.volume, v.runtime)
		}
	}
	if containers.volumes[0].volume != "new-dev-home" {
		t.Errorf("volume name = %q, want new-dev-home", containers.volumes[0].volume)
	}
}

// The short label is what someone types at a prompt, and what agentctl finds
// this machine's boxes with. The qualified keys stay for the agent itself.
func TestContainerCarriesTheShortPlatformLabel(t *testing.T) {
	containers := &fakeContainers{}
	svc, _ := testService(t, containers)

	if _, err := svc.Ensure(context.Background(), runtimeFor("new-dev")); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if containers.created == nil {
		t.Fatal("nothing was created")
	}
	labels := containers.created.Labels
	if labels[LabelPlatform] != "nikolai-laptop" {
		t.Errorf("%s = %q, want the location this machine serves", LabelPlatform, labels[LabelPlatform])
	}
	if labels[LabelPlatform] != labels[LabelLocation] {
		t.Errorf("the two spellings disagree: %q vs %q", labels[LabelPlatform], labels[LabelLocation])
	}
}
