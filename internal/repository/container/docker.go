package container

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	runtimesvc "scm.x5.ru/dis.cloud/core/location-agent/internal/services/runtime"
)

// Docker drives the local engine through its CLI.
//
// The CLI rather than the Engine API on purpose: it already knows where the
// socket is under Docker Desktop, honours `docker context`, and needs no
// vendored client. The repository interface keeps that a private decision.
type Docker struct {
	binary string
	bind   string
	log    *slog.Logger
}

func NewDocker(binary, bindAddress string, log *slog.Logger) *Docker {
	if binary == "" {
		binary = "docker"
	}
	return &Docker{binary: binary, bind: bindAddress, log: log}
}

func (d *Docker) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return "", fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return string(out), nil
}

// inspected is the slice of `docker inspect` output this agent relies on.
type inspected struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

func (d *Docker) Find(ctx context.Context, name string) (runtimesvc.Observed, bool, error) {
	out, err := d.run(ctx, "inspect", "--type", "container", "--format", "{{json .}}", name)
	if err != nil {
		// Absent is an answer, not a failure. Matched case-insensitively:
		// docker says "No such container" here and "No such object" for other
		// types, and the exact spelling is not a contract.
		if isNotFound(err) {
			return runtimesvc.Observed{}, false, nil
		}
		return runtimesvc.Observed{}, false, err
	}
	obs, err := parseInspect(out)
	if err != nil {
		return runtimesvc.Observed{}, false, err
	}
	return obs, true, nil
}

func (d *Docker) List(ctx context.Context) ([]runtimesvc.Observed, error) {
	out, err := d.run(ctx, "ps", "--all", "--filter", "label="+runtimesvc.LabelRuntime, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var observed []runtimesvc.Observed
	for _, name := range strings.Fields(out) {
		o, found, err := d.Find(ctx, name)
		if err != nil {
			return nil, err
		}
		if found {
			observed = append(observed, o)
		}
	}
	return observed, nil
}

// EnsureVolume creates the volume if it is missing, tagged with the runtime it
// belongs to. `docker volume create` is already idempotent, so there is nothing
// to check first.
//
// The label is what makes removal exact later. Volume names are
// "<runtime>-<declared name>", and reading ownership back out of that string is
// guesswork: "new-dev-home" belongs to runtime "new-dev", but nothing in the
// name rules out a runtime "new" with a volume "dev-home".
//
// Docker ignores the label when the volume already exists, so volumes made
// before this agent learned to label them stay unlabelled. Removal covers those
// through the container's own mount list.
func (d *Docker) EnsureVolume(ctx context.Context, runtime, name string) error {
	_, err := d.run(ctx, "volume", "create", "--label", runtimesvc.LabelRuntime+"="+runtime, name)
	return err
}

// volumesOf lists the named volumes a container mounts. This is the one source
// that cannot be wrong: it is what the container actually uses, whatever the
// template said when it was created or says now.
func (d *Docker) volumesOf(ctx context.Context, name string) ([]string, error) {
	out, err := d.run(ctx, "inspect", "--type", "container", "--format",
		`{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}`, name)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return strings.Fields(out), nil
}

// volumesLabelled lists volumes tagged as belonging to a runtime. It covers
// what volumesOf cannot: a container someone removed by hand, and volumes an
// earlier version of the template declared and the current one no longer does.
func (d *Docker) volumesLabelled(ctx context.Context, runtime string) ([]string, error) {
	out, err := d.run(ctx, "volume", "ls", "--quiet", "--filter", "label="+runtimesvc.LabelRuntime+"="+runtime)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func (d *Docker) Create(ctx context.Context, c runtimesvc.Container) (runtimesvc.Observed, error) {
	if c.Pull {
		// A pull failure is not fatal when the image is already here: locally
		// built images — claude-box among them — exist in no registry.
		if _, err := d.run(ctx, "pull", c.Image); err != nil {
			if _, e := d.run(ctx, "image", "inspect", c.Image); e != nil {
				return runtimesvc.Observed{}, fmt.Errorf("pull %s: %w", c.Image, err)
			}
			d.log.Warn("pull failed, using the local image", "image", c.Image, "error", err)
		}
	}

	if _, err := d.run(ctx, append([]string{"run"}, RunArgs(c, d.bind)...)...); err != nil {
		return runtimesvc.Observed{}, err
	}
	obs, found, err := d.Find(ctx, c.Name)
	if err != nil {
		return runtimesvc.Observed{}, err
	}
	if !found {
		return runtimesvc.Observed{}, fmt.Errorf("container %q vanished right after being created", c.Name)
	}
	return obs, nil
}

func (d *Docker) Start(ctx context.Context, name string) error {
	_, err := d.run(ctx, "start", name)
	return err
}

// Remove deletes the container and, when asked, the volumes that belong to it.
//
// The volumes are collected before the container goes: afterwards its mount
// list is gone with it, and the only remaining evidence of ownership is the
// label. Both sources are used because neither covers everything — a container
// removed by hand leaves only labels, and a volume created before this agent
// labelled them appears only in the mount list.
//
// What is deliberately not used is the volume name. The previous version
// matched `name=<runtime>-`, which docker treats as a substring: deleting
// runtime "new" would have taken "new-dev-home" with it.
func (d *Docker) Remove(ctx context.Context, name string, purgeVolumes bool) error {
	var doomed []string
	if purgeVolumes {
		mounted, err := d.volumesOf(ctx, name)
		if err != nil {
			return err
		}
		labelled, err := d.volumesLabelled(ctx, name)
		if err != nil {
			return err
		}
		doomed = union(mounted, labelled)
	}

	_, found, err := d.Find(ctx, name)
	if err != nil {
		return err
	}
	if found {
		if _, err := d.run(ctx, "rm", "--force", name); err != nil {
			return err
		}
	}

	for _, v := range doomed {
		if _, err := d.run(ctx, "volume", "rm", v); err != nil {
			// Already gone is the state we wanted. Anything else — a volume
			// another container still holds, most likely — is reported, and the
			// reconcile retries.
			if isNotFound(err) {
				continue
			}
			return fmt.Errorf("remove volume %s: %w", v, err)
		}
		d.log.Info("removed runtime volume", "runtime", name, "volume", v)
	}
	return nil
}

// union merges the two volume sources, keeping the order stable so logs and
// tests read the same way twice.
func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{a, b} {
		for _, v := range list {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// RunArgs builds the argument list for `docker run`.
//
// Exported so the mapping can be tested without an engine: getting a flag
// wrong here produces a container that is subtly different from what the
// template asked for, which is not something the type system catches.
func RunArgs(c runtimesvc.Container, bindAddress string) []string {
	args := []string{"--detach", "--name", c.Name, "--restart", "unless-stopped"}

	for _, k := range sortedKeys(c.Labels) {
		args = append(args, "--label", k+"="+c.Labels[k])
	}
	for _, k := range sortedKeys(c.Env) {
		args = append(args, "--env", k+"="+c.Env[k])
	}

	ports := append([]runtimesvc.Port(nil), c.Ports...)
	sort.Slice(ports, func(i, j int) bool { return ports[i].Name < ports[j].Name })
	for _, p := range ports {
		if p.Host == 0 {
			continue
		}
		args = append(args, "--publish",
			fmt.Sprintf("%s:%d:%d", bindAddress, p.Host, p.Container))
	}

	mounts := append([]runtimesvc.Mount(nil), c.Mounts...)
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Path < mounts[j].Path })
	for _, m := range mounts {
		// A host path and a volume name go in the same place on the command
		// line: docker tells them apart by the leading slash.
		source := m.Volume
		if m.Host != "" {
			source = m.Host
		}
		spec := source + ":" + m.Path
		if m.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "--volume", spec)
	}

	if c.Workdir != "" {
		args = append(args, "--workdir", c.Workdir)
	}
	if c.CPU != "" {
		args = append(args, "--cpus", c.CPU)
	}
	if mem := dockerMemory(c.Memory); mem != "" {
		args = append(args, "--memory", mem)
	}

	return append(args, c.Image)
}

// dockerMemory converts a Kubernetes quantity into what docker accepts.
// "2Gi" is meaningless to docker; "2g" is the same amount.
func dockerMemory(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	for _, s := range []struct{ k8s, docker string }{
		{"Gi", "g"}, {"Mi", "m"}, {"Ki", "k"},
		{"G", "g"}, {"M", "m"}, {"K", "k"},
	} {
		if strings.HasSuffix(q, s.k8s) {
			return strings.TrimSuffix(q, s.k8s) + s.docker
		}
	}
	return q
}

func parseInspect(out string) (runtimesvc.Observed, error) {
	var in inspected
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &in); err != nil {
		return runtimesvc.Observed{}, fmt.Errorf("parse docker inspect: %w", err)
	}
	obs := runtimesvc.Observed{
		Name:    strings.TrimPrefix(in.Name, "/"),
		ID:      in.ID,
		Image:   in.Config.Image,
		State:   in.State.Status,
		Running: in.State.Running,
		Labels:  in.Config.Labels,
		Ports:   map[string]int{},
		Binds:   map[string]string{},
	}
	// Bind mounts are read back so a VolumeMount can say whether it is actually
	// on the container, rather than whether we asked for it. The two differ for
	// as long as a recreate takes, and that is exactly the window someone
	// watching the resource is in.
	for _, m := range in.Mounts {
		if m.Type != "bind" {
			continue
		}
		obs.Binds[m.Destination] = m.Source
	}
	// Port assignments are read back from our own labels rather than from the
	// engine's bindings: the label carries the port's name, which is what the
	// template and the status speak in.
	for k, v := range in.Config.Labels {
		if !strings.HasPrefix(k, runtimesvc.LabelPort) {
			continue
		}
		port, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		obs.Ports[strings.TrimPrefix(k, runtimesvc.LabelPort)] = port
	}
	return obs, nil
}

// isNotFound reports whether docker means "it is not there".
func isNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "no such object") ||
		strings.Contains(msg, "no such image")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
