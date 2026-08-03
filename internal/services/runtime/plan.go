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
	"sort"
	"strings"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
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

// Port is one published port.
type Port struct {
	Name string
	// Container is the port inside the container, from the template.
	Container int
	// Host is what it is published on. Assigned by the allocator.
	Host int
	HTTP bool
}

// Mount is storage attached to the container. Exactly one of Volume or Tmpfs
// applies: a named volume outlives the container, a tmpfs does not.
type Mount struct {
	// Volume is the docker volume name, empty for a tmpfs mount.
	Volume string
	Path   string
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

	// Environment, weakest source first: the template describes the image, the
	// user config personalises it, and the platform's own settings must win —
	// a user config cannot be allowed to point a runtime at another shard.
	for _, e := range spec.Container.Env {
		c.Env[e.Name] = e.Value
	}
	if in.UserConfig != nil {
		for k, v := range in.UserConfig.Spec.Env {
			c.Env[k] = v
		}
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
