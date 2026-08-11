// Package runtime turns a Runtime plus its template and user config into the
// container that should exist for it.
//
// The planning here is pure: no docker, no control plane, no clock. That is
// what makes it testable, and the mapping from the platform's Kubernetes
// vocabulary onto docker is exactly the part worth testing.
//
// The resource types come from the repositories that own them — the platform's
// own api packages — rather than being restated here. A second copy of a
// schema does not merely risk drifting: fields the copy never heard of are
// dropped in silence, which is a bug with no symptom until someone uses one.
package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
	agentapi "scm.x5.ru/dis.cloud/core/location-agent/api"
	pa "scm.x5.ru/dis.cloud/core/provision-agent/api"
)

// Group is the platform's resource group, used to namespace our labels.
const Group = platform.RuntimeGroup

// Label keys carried on every container this agent creates. They are the only
// state it keeps: docker is asked what exists rather than a file on disk being
// trusted to still describe reality.
const (
	LabelRuntime  = Group + "/runtime"
	LabelLocation = Group + "/location"
	LabelSpecHash = Group + "/spec-hash"
	LabelPort     = Group + "/port."

	// LabelPlatform carries the shard this machine answers to — the same value
	// as LabelLocation, under a key short enough to type.
	//
	// It exists for people and their tools, not for this agent: the qualified
	// keys above are unambiguous but unusable at a prompt, and
	// `docker ps --filter label=agent-platform=my-laptop` is what someone
	// actually wants to run. agentctl finds this machine's boxes with it.
	LabelPlatform = "agent-platform"
)

// A box gets a few ports of its own, published with the same number inside and
// out. The point is the sameness: something started on 19080 in a box is on
// 19080 on the machine, so the number the box prints is the number that works,
// and nobody has to translate.
//
// Their names are ours, not a template's — no template declares them — and they
// are what keeps an allocation stable: a running container's ports are read
// back by name, so a box keeps the ones it already has.
const (
	// LocalPortPrefix names them. Kept distinct from anything a template would
	// call a port, which is a word like "ssh" or "api".
	LocalPortPrefix = "local-"

	// LocalPortEnv tells the box which ones it got, in the order they were
	// asked for. Read inside, so it lists what to bind to there.
	LocalPortEnv = "LOCAL_CONTAINER_PORT_RANGE"

	// localPortSearchSpan bounds the search above the first port. Wide enough
	// for many boxes on one machine, narrow enough that the numbers stay short.
	localPortSearchSpan = 919
)

// LocalPortName is the name the nth local port is recorded under.
func LocalPortName(i int) string { return LocalPortPrefix + strconv.Itoa(i) }

// Port is one published port.
type Port struct {
	Name string
	// Container is the port inside the container, from the template.
	Container int
	// Host is what it is published on. Assigned by the allocator.
	Host int
	HTTP bool
}

// Mount is storage attached to the container. Exactly one of Volume, Host or
// Tmpfs applies: a named volume outlives the container, a host directory
// belongs to the machine and outlives everything, a tmpfs does not.
type Mount struct {
	// Volume is the docker volume name, empty for the other two kinds.
	Volume string
	// Host is a directory on the machine, bound in as it is. Set only by a
	// VolumeMount resource — a template cannot ask for one, because it is
	// written once for every location and a path on one laptop means nothing on
	// the next.
	//
	// omitempty is load-bearing: the whole Mount goes into the spec hash, so
	// without it every container that predates this field would hash
	// differently the moment the agent was upgraded, and every box on every
	// machine would be recreated once for a field none of them use.
	Host string `json:"host,omitempty"`
	Path string
	// Tmpfs marks an emptyDir, which has no counterpart that survives.
	Tmpfs bool
	// SizeLimit is a tmpfs size, passed through from the template.
	SizeLimit string
	ReadOnly  bool
}

// Container is the desired state of one runtime's container.
type Container struct {
	Name    string
	Image   string
	Pull    bool
	Workdir string
	CPU     string
	Memory  string
	// Command and Args override the image's entrypoint, as in Kubernetes.
	Command []string
	Args    []string
	Env     map[string]string
	Ports   []Port
	Mounts  []Mount
	Labels  map[string]string
}

// Input is everything planning needs. Kept explicit so the caller does the
// fetching and this stays a function of its arguments.
type Input struct {
	Runtime    *platform.Runtime
	Template   *platform.RuntimeTemplate
	UserConfig *pa.UserConfig
	// Location is the shard this agent serves, recorded on the container.
	Location string
	// AgentEnv is what the runtime's own provision-agent needs to reach the
	// control plane. Supplied by the caller because it is this machine's
	// configuration, not the template's.
	AgentEnv map[string]string

	// LocalUserConfig is the machine's own profile, passed to the box verbatim.
	LocalUserConfig string
	// HostMounts are the VolumeMount resources pointing at this runtime. The
	// caller has already dropped the ones it could not honour, so everything
	// here is meant to end up on the container.
	HostMounts []*agentapi.VolumeMount
}

// Plan computes the container a Runtime should have.
//
// Host ports are left at zero: which port is free is a property of the machine
// at this moment, not of the desired state, so the allocator fills them in
// afterwards and the hash below deliberately ignores them.
func Plan(in Input) (Container, error) {
	if in.Runtime == nil {
		return Container{}, fmt.Errorf("runtime is nil")
	}
	name := in.Runtime.Name
	if strings.TrimSpace(name) == "" {
		return Container{}, fmt.Errorf("runtime has no name")
	}
	if in.Template == nil {
		return Container{}, fmt.Errorf("runtime %q: template is not resolved", name)
	}
	spec := in.Template.Spec
	if strings.TrimSpace(spec.Container.Image) == "" {
		return Container{}, fmt.Errorf("runtime %q: template %q has no image", name, in.Template.Name)
	}

	// envFrom pulls values out of Kubernetes secrets and config maps. There is
	// nothing here to pull them from, and quietly starting a container without
	// them would produce a failure far from its cause.
	if len(spec.Container.EnvFrom) > 0 {
		return Container{}, fmt.Errorf(
			"runtime %q: template %q uses envFrom, which needs Kubernetes secrets or config maps",
			name, in.Template.Name)
	}

	c := Container{
		Name:    name,
		Image:   spec.Container.Image,
		Pull:    spec.Container.ImagePullPolicy == "Always",
		Workdir: spec.Container.WorkingDir,
		CPU:     spec.Container.Resources.CPU,
		Memory:  spec.Container.Resources.Memory,
		Command: spec.Container.Command,
		Args:    spec.Container.Args,
		Env:     map[string]string{},
		Labels: map[string]string{
			LabelRuntime:  name,
			LabelLocation: in.Location,
			LabelPlatform: in.Location,
		},
	}

	// A runtime may ask for more than the template's defaults.
	if in.Runtime.Spec.Resources.CPU != "" {
		c.CPU = in.Runtime.Spec.Resources.CPU
	}
	if in.Runtime.Spec.Resources.Memory != "" {
		c.Memory = in.Runtime.Spec.Resources.Memory
	}

	// Environment, weakest source first.
	//
	// The template describes the image, so it goes in first. The user config
	// personalises every box the user has. The template's own parameters —
	// what its envSchema asked for, answered by this user — are more specific
	// than either, so they land on top of both.
	//
	// The platform's own settings win over all of it: a user config cannot be
	// allowed to point a runtime at another shard.
	for _, e := range spec.Container.Env {
		c.Env[e.Name] = e.Value
	}
	if in.UserConfig != nil {
		for k, v := range in.UserConfig.Spec.Env {
			c.Env[k] = v
		}
		for k, v := range in.UserConfig.Spec.RuntimeEnvParams[in.Template.Name] {
			c.Env[k] = v
		}
	}
	// The machine's own profile, for the agent inside to apply to what it reads
	// itself. Under the platform's settings, like everything else here: a
	// profile must not be able to repoint a box at another control plane.
	if in.LocalUserConfig != "" {
		c.Env[localUserConfigEnv] = in.LocalUserConfig
	}
	for k, v := range in.AgentEnv {
		c.Env[k] = v
	}

	ports, err := mergePorts(name, spec.Container.Ports, in.Runtime.Spec.Ports)
	if err != nil {
		return Container{}, err
	}
	c.Ports = ports

	mounts, err := planMounts(name, spec.Volumes, spec.Container.VolumeMounts)
	if err != nil {
		return Container{}, err
	}
	mounts, err = addHostMounts(name, mounts, in.HostMounts)
	if err != nil {
		return Container{}, err
	}
	c.Mounts = mounts

	c.Labels[LabelSpecHash] = c.Hash()
	return c, nil
}

// mergePorts combines the template's ports with any the runtime asks for on
// top. A runtime's entry replaces the template's of the same name, which is
// how a runtime opens an extra port without forking the template.
func mergePorts(runtime string, fromTemplate, fromRuntime []platform.PortSpec) ([]Port, error) {
	order := make([]string, 0, len(fromTemplate)+len(fromRuntime))
	byName := map[string]platform.PortSpec{}
	for _, p := range append(append([]platform.PortSpec{}, fromTemplate...), fromRuntime...) {
		if strings.TrimSpace(p.Name) == "" {
			return nil, fmt.Errorf("runtime %q: a port has no name", runtime)
		}
		if _, seen := byName[p.Name]; !seen {
			order = append(order, p.Name)
		}
		byName[p.Name] = p
	}

	out := make([]Port, 0, len(order))
	for _, name := range order {
		p := byName[name]
		if p.Port <= 0 {
			return nil, fmt.Errorf("runtime %q: port %q has no container port", runtime, name)
		}
		out = append(out, Port{
			Name:      p.Name,
			Container: int(p.Port),
			HTTP:      p.Protocol == platform.PortProtocolHTTP,
		})
	}
	return out, nil
}

// planMounts maps the template's volumes onto docker.
//
// A persistent claim becomes a named volume of this runtime's own, so two
// runtimes on one machine never share a home directory by accident. An
// emptyDir becomes a tmpfs, which matches its "gone with the container"
// semantics. Secrets and config maps have no local equivalent and are refused
// rather than skipped.
func planMounts(runtime string, volumes []platform.Volume, mounts []platform.VolumeMount) ([]Mount, error) {
	declared := map[string]platform.Volume{}
	for _, v := range volumes {
		declared[v.Name] = v
	}

	out := make([]Mount, 0, len(mounts))
	for _, m := range mounts {
		v, ok := declared[m.Name]
		if !ok {
			return nil, fmt.Errorf("runtime %q: volumeMount %q has no matching volume", runtime, m.Name)
		}
		if m.SubPath != "" {
			return nil, fmt.Errorf("runtime %q: volumeMount %q uses subPath, which is not supported here", runtime, m.Name)
		}

		switch {
		case v.PersistentVolumeClaim != nil:
			out = append(out, Mount{
				Volume:   VolumeName(runtime, v.Name),
				Path:     m.MountPath,
				ReadOnly: m.ReadOnly,
			})
		case v.EmptyDir != nil:
			out = append(out, Mount{
				Path:      m.MountPath,
				Tmpfs:     true,
				SizeLimit: v.EmptyDir.SizeLimit,
				ReadOnly:  m.ReadOnly,
			})
		case v.Secret != nil || v.ConfigMap != nil:
			return nil, fmt.Errorf(
				"runtime %q: volume %q is a secret or config map, which needs Kubernetes", runtime, v.Name)
		default:
			return nil, fmt.Errorf("runtime %q: volume %q declares no source", runtime, v.Name)
		}
	}
	return out, nil
}

// addHostMounts puts the machine's own directories on top of the template's
// storage.
//
// Sorted by resource name so two mounts never swap places between reconciles:
// the order reaches the spec hash, and a hash that moves on its own would
// recreate the container forever.
//
// A path already used by the template is refused rather than layered over.
// Docker would accept it and mount the host directory on top, which is how a
// box loses sight of its own home directory — and the symptom is an empty
// workspace, nowhere near the cause.
func addHostMounts(runtime string, mounts []Mount, hostMounts []*agentapi.VolumeMount) ([]Mount, error) {
	if len(hostMounts) == 0 {
		return mounts, nil
	}

	taken := map[string]string{}
	for _, m := range mounts {
		taken[filepath.Clean(m.Path)] = "the template"
	}

	sorted := append([]*agentapi.VolumeMount(nil), hostMounts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	for _, hm := range sorted {
		if err := ValidateMount(hm.Spec.HostPath, hm.Spec.ContainerPath); err != nil {
			return nil, fmt.Errorf("runtime %q: volume mount %q: %w", runtime, hm.Name, err)
		}
		path := filepath.Clean(hm.Spec.ContainerPath)
		if owner, clash := taken[path]; clash {
			return nil, fmt.Errorf("runtime %q: volume mount %q wants %s, which %s already uses",
				runtime, hm.Name, path, owner)
		}
		taken[path] = "volume mount " + hm.Name
		mounts = append(mounts, Mount{
			Host:     filepath.Clean(hm.Spec.HostPath),
			Path:     path,
			ReadOnly: hm.Spec.ReadOnly,
		})
	}
	return mounts, nil
}

// ValidateMount checks what can be judged without touching the machine, so the
// same rules apply whether a mount is being planned or its resource is being
// reported on.
//
// Whether the host directory exists is deliberately not checked here: that is a
// question about the machine at this moment, and this file is a function of its
// arguments.
func ValidateMount(hostPath, containerPath string) error {
	if strings.TrimSpace(hostPath) == "" {
		return fmt.Errorf("hostPath is empty")
	}
	if strings.TrimSpace(containerPath) == "" {
		return fmt.Errorf("containerPath is empty")
	}
	if !filepath.IsAbs(hostPath) {
		return fmt.Errorf("hostPath %q is not absolute", hostPath)
	}
	if !filepath.IsAbs(containerPath) {
		return fmt.Errorf("containerPath %q is not absolute", containerPath)
	}
	// "/" would shadow the whole filesystem of the box, including the agent
	// that serves it. Nothing good follows from allowing it.
	if filepath.Clean(containerPath) == "/" {
		return fmt.Errorf("containerPath %q would replace the container's root", containerPath)
	}
	return nil
}

// localUserConfigEnv is where the agent inside a box looks for the profile this
// machine added. The name is provision-agent's; it is written here because this
// is the side that fills it in.
const localUserConfigEnv = "LOCAL_USER_CONFIG"

// VolumeName is the docker volume backing one of a runtime's declared volumes.
func VolumeName(runtime, volume string) string { return runtime + "-" + volume }

// Hash fingerprints the desired state, so a reconcile can tell "already what
// we want" from "must be recreated" without comparing everything by hand.
//
// Host ports and the hash label itself are excluded: the former are assigned
// by the machine, and including the latter would be circular.
func (c Container) Hash() string {
	type port struct {
		Name      string `json:"name"`
		Container int    `json:"container"`
		HTTP      bool   `json:"http"`
	}
	payload := struct {
		Image   string            `json:"image"`
		Workdir string            `json:"workdir"`
		CPU     string            `json:"cpu"`
		Memory  string            `json:"memory"`
		Command []string          `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
		Ports   []port            `json:"ports"`
		Mounts  []Mount           `json:"mounts"`
	}{
		Image:   c.Image,
		Workdir: c.Workdir,
		CPU:     c.CPU,
		Memory:  c.Memory,
		Command: c.Command,
		Args:    c.Args,
		Env:     c.Env,
		Mounts:  append([]Mount(nil), c.Mounts...),
	}
	for _, p := range c.Ports {
		payload.Ports = append(payload.Ports, port{Name: p.Name, Container: p.Container, HTTP: p.HTTP})
	}
	// Order must not affect the fingerprint, or an unchanged runtime would be
	// recreated whenever a map iterated differently.
	sort.Slice(payload.Ports, func(i, j int) bool { return payload.Ports[i].Name < payload.Ports[j].Name })
	sort.Slice(payload.Mounts, func(i, j int) bool { return payload.Mounts[i].Path < payload.Mounts[j].Path })

	raw, err := json.Marshal(payload)
	if err != nil {
		// Marshalling maps of strings cannot fail; treat it as "always differs"
		// rather than pretending the state is known.
		return "unhashable"
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
