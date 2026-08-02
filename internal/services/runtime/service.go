package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
	pa "scm.x5.ru/dis.cloud/core/provision-agent/api"
)

// ResourceRepository reads the platform resources a runtime refers to.
type ResourceRepository interface {
	RuntimeTemplate(ctx context.Context, name string) (*platform.RuntimeTemplate, bool, error)
	UserConfig(ctx context.Context, name string) (*pa.UserConfig, bool, error)
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
	// AgentEnv is handed to every container so the agent inside can reach the
	// control plane. Runtime-specific values are added on top.
	AgentEnv map[string]string
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

	ports, err := s.assignPorts(ctx, rt.Name, tpl, existing, hasExisting)
	if err != nil {
		return State{}, err
	}

	desired, err := Plan(Input{
		Runtime:    rt,
		Template:   tpl,
		UserConfig: userConfig,
		Location:   s.cfg.Location,
		AgentEnv:   s.agentEnv(rt, ports),
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
	return env
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

	if hasExisting {
		for name, p := range existing.Ports {
			assigned[name] = p
			taken[p] = true
		}
	}

	// Sorted so a fresh runtime gets the same ports on every run, which makes
	// the result reproducible and the logs comparable.
	wanted := append([]platform.PortSpec(nil), tpl.Spec.Container.Ports...)
	sort.Slice(wanted, func(i, j int) bool { return wanted[i].Name < wanted[j].Name })

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
	return assigned, nil
}

// freePort finds a port that is neither claimed by another runtime nor in use
// by something else on this machine. Checking the machine matters: a laptop
// runs plenty that the agent knows nothing about.
func (s *Service) freePort(taken map[int]bool) (int, error) {
	for p := s.cfg.PortMin; p <= s.cfg.PortMax; p++ {
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
	return 0, fmt.Errorf("no free port in %d-%d", s.cfg.PortMin, s.cfg.PortMax)
}

func applyHostPorts(c *Container, ports map[string]int) {
	for i := range c.Ports {
		host := ports[c.Ports[i].Name]
		c.Ports[i].Host = host
		c.Labels[LabelPort+c.Ports[i].Name] = strconv.Itoa(host)
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
