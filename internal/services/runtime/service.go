package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
	agentapi "scm.x5.ru/dis.cloud/core/location-agent/api"
	pa "scm.x5.ru/dis.cloud/core/provision-agent/api"
)

// ResourceRepository reads the platform resources a runtime refers to.
type ResourceRepository interface {
	RuntimeTemplate(ctx context.Context, name string) (*platform.RuntimeTemplate, bool, error)
	UserConfig(ctx context.Context, name string) (*pa.UserConfig, bool, error)
	// VolumeMounts are the host directories asked for this runtime, which the
	// runtime itself knows nothing about — they are declared next to it.
	VolumeMounts(ctx context.Context, namespace, runtime string) ([]*agentapi.VolumeMount, error)
}

// Observed is a container as it currently exists.
type Observed struct {
	Name    string
	ID      string
	Image   string
	State   string
	Running bool
	Labels  map[string]string
	// Ports maps a port name to the host port it is published on.
	Ports map[string]int
	// Binds maps a path inside the container to the host directory bound
	// there. Only bind mounts appear — named volumes are not host directories
	// and nobody asks after them by path.
	Binds map[string]string
}

// SpecHash returns the fingerprint the container was created from, or "" when
// it carries none — a container from before this agent, or from another tool.
func (o Observed) SpecHash() string { return o.Labels[LabelSpecHash] }

// ContainerRepository is the machine's container engine.
type ContainerRepository interface {
	Find(ctx context.Context, name string) (Observed, bool, error)
	List(ctx context.Context) ([]Observed, error)
	EnsureVolume(ctx context.Context, runtime, name string) error
	Create(ctx context.Context, c Container) (Observed, error)
	Start(ctx context.Context, name string) error
	Remove(ctx context.Context, name string, purgeVolumes bool) error
}

// Config is what this machine contributes to every runtime it hosts.
type Config struct {
	Location     string
	BindAddress  string
	ExternalHost string
	PortMin      int
	PortMax      int
	// LocalPortMin and LocalPortCount are the ports every box gets for whatever
	// its user runs inside. Published with the same number on both sides, so
	// the address the box reports is the address the machine answers on.
	LocalPortMin   int
	LocalPortCount int
	// AgentEnv is handed to every container so the agent inside can reach the
	// control plane. Runtime-specific values are added on top.
	AgentEnv map[string]string

	// LocalUserConfig is the profile this machine keeps, as it was written on
	// disk. Handed to each box so the agent inside adds the same thing to the
	// git credentials and MCP values it reads for itself — this agent only
	// applies it to the environment and the files.
	LocalUserConfig string
}

// ProvisionService keeps the runtime's Provision resource in step. Declared
// here as an interface so the runtime service depends on what it needs rather
// than on another service's type.
type ProvisionService interface {
	Ensure(ctx context.Context, runtime string, defaults platform.ProvisionDefaults, userConfig *pa.UserConfig) error
	Remove(ctx context.Context, runtime string) error
}

type Service struct {
	cfg        Config
	resources  ResourceRepository
	containers ContainerRepository
	provision  ProvisionService
	log        *slog.Logger
}

func New(cfg Config, resources ResourceRepository, containers ContainerRepository, provision ProvisionService, log *slog.Logger) *Service {
	return &Service{cfg: cfg, resources: resources, containers: containers, provision: provision, log: log}
}

// State is what the reconciler writes back to the resource.
//
// There is no field for the container itself: the platform's Runtime status
// describes a pod and a service, and inventing one here would be another
// schema only this agent knows about. The container is named after the runtime
// anyway, and its health shows up as the phase.
type State struct {
	TCP     []platform.TCPRoute
	Ingress []platform.IngressHost
	Running bool
}

// Ensure makes the machine match the Runtime and reports what exists.
func (s *Service) Ensure(ctx context.Context, rt *platform.Runtime) (State, error) {
	tpl, found, err := s.resources.RuntimeTemplate(ctx, rt.Spec.TemplateRef.Name)
	if err != nil {
		return State{}, err
	}
	if !found {
		return State{}, fmt.Errorf("runtime template %q not found", rt.Spec.TemplateRef.Name)
	}

	// A missing user config is not fatal: it only personalises the runtime.
	// It is worth saying out loud though — a runtime that references one and
	// silently gets nothing starts up fine and then cannot reach anything that
	// needs credentials.
	userConfig, found, err := s.resources.UserConfig(ctx, rt.Spec.UserConfigRef.Name)
	if err != nil {
		return State{}, err
	}
	if rt.Spec.UserConfigRef.Name != "" && !found {
		s.log.Warn("user config not found — the runtime will start without its environment",
			"runtime", rt.Name, "userConfig", rt.Spec.UserConfigRef.Name)
	}

	// Written before the container starts: the agent inside reads it as soon
	// as it comes up, and a runtime that boots without one has to wait for the
	// next reconcile to learn where its skills go.
	if err := s.provision.Ensure(ctx, rt.Name, tpl.Spec.ProvisionDefaults, userConfig); err != nil {
		return State{}, err
	}

	existing, hasExisting, err := s.containers.Find(ctx, rt.Name)
	if err != nil {
		return State{}, err
	}

	hostMounts, err := s.hostMounts(ctx, rt)
	if err != nil {
		return State{}, err
	}

	ports, err := s.assignPorts(ctx, rt.Name, tpl, existing, hasExisting)
	if err != nil {
		return State{}, err
	}

	desired, err := Plan(Input{
		Runtime:         rt,
		Template:        tpl,
		UserConfig:      userConfig,
		Location:        s.cfg.Location,
		AgentEnv:        s.agentEnv(rt, ports),
		LocalUserConfig: s.cfg.LocalUserConfig,
		HostMounts:      hostMounts,
	})
	if err != nil {
		return State{}, err
	}
	applyHostPorts(&desired, ports)

	if hasExisting && existing.SpecHash() == desired.Labels[LabelSpecHash] {
		if !existing.Running {
			if err := s.containers.Start(ctx, rt.Name); err != nil {
				return State{}, err
			}
			existing.Running = true
			existing.State = "running"
		}
		return s.state(existing, desired), nil
	}

	if hasExisting {
		// Recreate rather than mutate: a container's image, ports and mounts
		// are fixed at creation, and half-applying a change is worse than a
		// brief gap. Volumes survive, so nothing the user cares about is lost.
		s.log.Info("recreating runtime container",
			"runtime", rt.Name, "was", existing.SpecHash(), "want", desired.Labels[LabelSpecHash])
		if err := s.containers.Remove(ctx, rt.Name, false); err != nil {
			return State{}, err
		}
	}

	for _, m := range desired.Mounts {
		// Only named volumes are ours to create. A host directory already
		// exists — that is the whole point of one — and a tmpfs is made by
		// docker at start.
		if m.Volume == "" {
			continue
		}
		if err := s.containers.EnsureVolume(ctx, rt.Name, m.Volume); err != nil {
			return State{}, err
		}
	}

	created, err := s.containers.Create(ctx, desired)
	if err != nil {
		return State{}, err
	}
	return s.state(created, desired), nil
}

// Remove tears the runtime down: the container and the volumes that belong to
// it. Deleting a Runtime is how a box is disposed of, so it leaves nothing on
// the machine — otherwise every deleted box keeps its disk, and a runtime later
// created under the same name silently inherits the old one's work directory
// and its enrolled ssh keys.
//
// This is not recoverable. The workspace and the agent's home go with it,
// including whatever was never pushed anywhere. Recreating the same name gives
// a clean box, not the old one back. Restarting or changing a runtime does not
// come through here — Ensure recreates the container and keeps the volumes.
func (s *Service) Remove(ctx context.Context, rt *platform.Runtime) error {
	if err := s.containers.Remove(ctx, rt.Name, true); err != nil {
		return err
	}
	// The Provision outlives nothing: without its runtime it describes a
	// container that no longer exists.
	return s.provision.Remove(ctx, rt.Name)
}

// hostMounts collects the VolumeMounts for this runtime and drops the ones the
// machine cannot honour.
//
// Dropped rather than fatal, because the two failures are not the same size. A
// box with one directory missing is still a box someone can work in; refusing
// to reconcile the runtime over it would take the whole thing down over a typo
// in a resource that was added last. What went wrong is reported on the mount's
// own resource, which is where someone would look.
func (s *Service) hostMounts(ctx context.Context, rt *platform.Runtime) ([]*agentapi.VolumeMount, error) {
	all, err := s.resources.VolumeMounts(ctx, rt.Namespace, rt.Name)
	if err != nil {
		return nil, err
	}

	out := make([]*agentapi.VolumeMount, 0, len(all))
	for _, vm := range all {
		if err := CheckMount(vm.Spec.HostPath, vm.Spec.ContainerPath); err != nil {
			s.log.Warn("skipping volume mount",
				"runtime", rt.Name, "volumeMount", vm.Name,
				"hostPath", vm.Spec.HostPath, "error", err)
			continue
		}
		out = append(out, vm)
	}
	return out, nil
}

// agentEnv is what the runtime's own agent needs, on top of the template's.
func (s *Service) agentEnv(rt *platform.Runtime, ports map[string]int) map[string]string {
	env := map[string]string{}
	for k, v := range s.cfg.AgentEnv {
		env[k] = v
	}
	env["RUNTIME_NAME"] = rt.Name
	if rt.Spec.UserConfigRef.Name != "" {
		env["USER_CONFIG_NAME"] = rt.Spec.UserConfigRef.Name
	}
	// The container cannot see how its ports are published, so it is told.
	// Without this a client asking the runtime where to ssh gets the port the
	// daemon listens on inside, which is wrong everywhere outside.
	if p, ok := ports["ssh"]; ok {
		env["SSH_ADVERTISE_PORT"] = strconv.Itoa(p)
		env["SSH_ADVERTISE_HOST"] = s.cfg.ExternalHost
	}
	// The ports the box may use for its own work. Published with the same
	// number on both sides, so this list is both what to bind to inside and
	// what to open on the machine.
	if list := localPortList(ports, s.cfg.LocalPortCount); list != "" {
		env[LocalPortEnv] = list
	}
	return env
}

// localPortList is the box's own ports, in the order they were asked for.
// Ordered rather than sorted: "the first one" has to mean the same thing to
// whoever reads it and to whoever assigned it, and numerically they may not be
// consecutive when something else on the machine holds one.
func localPortList(ports map[string]int, count int) string {
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		p, ok := ports[LocalPortName(i)]
		if !ok {
			break
		}
		out = append(out, strconv.Itoa(p))
	}
	return strings.Join(out, ",")
}

// assignPorts picks host ports, keeping any a running container already has.
//
// Stability matters twice over: a changed port would rewrite the advertised
// address, and because that address is part of the container's environment it
// would also change the spec hash and trigger an endless recreate loop.
func (s *Service) assignPorts(ctx context.Context, runtime string, tpl *platform.RuntimeTemplate, existing Observed, hasExisting bool) (map[string]int, error) {
	assigned := map[string]int{}
	taken := map[int]bool{}

	others, err := s.containers.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range others {
		if o.Name == runtime {
			continue
		}
		for _, p := range o.Ports {
			taken[p] = true
		}
	}

	// What this runtime's own container already holds. Kept apart from taken:
	// its ports are not free, but they are not somebody else's either, and a
	// port it is sitting on must not read as a conflict with itself.
	mine := map[int]bool{}
	if hasExisting {
		for _, p := range existing.Ports {
			mine[p] = true
		}
	}

	// Sorted so a fresh runtime gets the same ports on every run, which makes
	// the result reproducible and the logs comparable.
	wanted := append([]platform.PortSpec(nil), tpl.Spec.Container.Ports...)
	sort.Slice(wanted, func(i, j int) bool { return wanted[i].Name < wanted[j].Name })

	// Pinned ports first, and before anything the running container holds: a
	// spec that asks for a number must get that number or an error. Letting the
	// old assignment win would recreate the container on every pass — the
	// number is in the spec hash, so the desired state would never be reached
	// and never stop being tried.
	for _, p := range wanted {
		if p.HostPort <= 0 {
			continue
		}
		host := int(p.HostPort)
		if err := s.checkPinned(host, taken, mine); err != nil {
			return nil, fmt.Errorf("runtime %q port %q: %w", runtime, p.Name, err)
		}
		assigned[p.Name] = host
		taken[host] = true
	}

	// Everything else keeps what it has, so an address already handed out does
	// not move under whoever is using it.
	if hasExisting {
		for name, p := range existing.Ports {
			if _, pinned := assigned[name]; pinned {
				continue
			}
			assigned[name] = p
			taken[p] = true
		}
	}

	for _, p := range wanted {
		if _, ok := assigned[p.Name]; ok {
			continue
		}
		host, err := s.freePort(taken)
		if err != nil {
			return nil, fmt.Errorf("runtime %q port %q: %w", runtime, p.Name, err)
		}
		assigned[p.Name] = host
		taken[host] = true
	}

	// The box's own ports, from their own range. Kept out of the pool above so
	// they stay low and memorable: somebody opens one in a browser, and 19080
	// is a better thing to be told than whatever the pool had spare.
	for i := 0; i < s.cfg.LocalPortCount; i++ {
		name := LocalPortName(i)
		if _, ok := assigned[name]; ok {
			continue
		}
		host, err := s.freePortIn(taken, s.cfg.LocalPortMin, s.cfg.LocalPortMin+localPortSearchSpan)
		if err != nil {
			return nil, fmt.Errorf("runtime %q port %q: %w", runtime, name, err)
		}
		assigned[name] = host
		taken[host] = true
	}
	return assigned, nil
}

// checkPinned reports whether a requested number can be published.
//
// A port this runtime's own container already holds is available to it: the
// listen test below would fail on it, because the container holding it is the
// one being reconciled.
func (s *Service) checkPinned(host int, taken, mine map[int]bool) error {
	if mine[host] {
		return nil
	}
	if taken[host] {
		return fmt.Errorf("port %d is asked for but another box on this machine has it", host)
	}
	l, err := net.Listen("tcp", net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(host)))
	if err != nil {
		return fmt.Errorf("port %d is asked for but something on this machine is using it", host)
	}
	_ = l.Close()
	return nil
}

// freePort finds a port that is neither claimed by another runtime nor in use
// by something else on this machine. Checking the machine matters: a laptop
// runs plenty that the agent knows nothing about.
func (s *Service) freePort(taken map[int]bool) (int, error) {
	return s.freePortIn(taken, s.cfg.PortMin, s.cfg.PortMax)
}

// freePortIn is the same search over an explicit range: the box's own ports
// come from their own, and mixing the two would put them wherever the pool
// happened to be free.
func (s *Service) freePortIn(taken map[int]bool, min, max int) (int, error) {
	for p := min; p <= max; p++ {
		if taken[p] {
			continue
		}
		l, err := net.Listen("tcp", net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(p)))
		if err != nil {
			continue
		}
		_ = l.Close()
		return p, nil
	}
	return 0, fmt.Errorf("no free port in %d-%d", min, max)
}

func applyHostPorts(c *Container, ports map[string]int) {
	for i := range c.Ports {
		host := ports[c.Ports[i].Name]
		c.Ports[i].Host = host
		c.Labels[LabelPort+c.Ports[i].Name] = strconv.Itoa(host)
	}

	// The box's own ports are added here rather than planned: their number
	// inside is the number they were given outside, and that is not known until
	// the allocator has run. Added in the order they were asked for, which is
	// the order the box is told about them.
	for i := 0; ; i++ {
		name := LocalPortName(i)
		host, ok := ports[name]
		if !ok {
			break
		}
		c.Ports = append(c.Ports, Port{Name: name, Container: host, Host: host})
		c.Labels[LabelPort+name] = strconv.Itoa(host)
	}
}

func (s *Service) state(o Observed, desired Container) State {
	st := State{Running: o.Running}
	for _, p := range desired.Ports {
		host := p.Host
		if actual, ok := o.Ports[p.Name]; ok && actual != 0 {
			host = actual
		}
		if host == 0 {
			continue
		}
		address := net.JoinHostPort(s.cfg.ExternalHost, strconv.Itoa(host))
		if p.HTTP {
			st.Ingress = append(st.Ingress, platform.IngressHost{
				PortName: p.Name,
				URL:      "http://" + address,
			})
			continue
		}
		st.TCP = append(st.TCP, platform.TCPRoute{
			PortName:     p.Name,
			Address:      address,
			ExternalHost: s.cfg.ExternalHost,
			ExternalPort: int32(host),
		})
	}
	return st
}
