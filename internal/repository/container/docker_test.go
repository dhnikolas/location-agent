package container

import (
	"strings"
	"testing"

	runtimesvc "scm.x5.ru/dis.cloud/core/location-agent/internal/services/runtime"
)

func argsString(args []string) string { return strings.Join(args, " ") }

func contains(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestRunArgs(t *testing.T) {
	c := runtimesvc.Container{
		Name:    "laptop-test",
		Image:   "registry/devcontainer:v1",
		Workdir: "/home/ubuntu/workspace",
		CPU:     "1",
		Memory:  "2Gi",
		Env:     map[string]string{"RUNTIME_NAME": "laptop-test", "A": "1"},
		Ports: []runtimesvc.Port{
			{Name: "ssh", Container: 22, Host: 31002},
			{Name: "api", Container: 8080, Host: 31001, HTTP: true},
		},
		Mounts: []runtimesvc.Mount{{Volume: "laptop-test-workspace", Path: "/home/ubuntu/workspace"}},
		Labels: map[string]string{runtimesvc.LabelRuntime: "laptop-test"},
	}

	args := RunArgs(c, "127.0.0.1")

	// The image must be last: anything after it is the container's own command.
	if args[len(args)-1] != c.Image {
		t.Errorf("image is not the final argument: %s", argsString(args))
	}
	if !contains(args, "--detach") || !contains(args, "--name", "laptop-test") {
		t.Errorf("missing basics: %s", argsString(args))
	}
	// Published on loopback, not on every interface: a runtime's API has no
	// authentication.
	if !contains(args, "--publish", "127.0.0.1:31002:22") {
		t.Errorf("ssh port not published on the bind address: %s", argsString(args))
	}
	if !contains(args, "--publish", "127.0.0.1:31001:8080") {
		t.Errorf("api port not published: %s", argsString(args))
	}
	if !contains(args, "--volume", "laptop-test-workspace:/home/ubuntu/workspace") {
		t.Errorf("volume not mounted: %s", argsString(args))
	}
	if !contains(args, "--workdir", "/home/ubuntu/workspace") {
		t.Errorf("workdir missing: %s", argsString(args))
	}
	if !contains(args, "--env", "RUNTIME_NAME=laptop-test") {
		t.Errorf("env missing: %s", argsString(args))
	}
	if !contains(args, "--label", runtimesvc.LabelRuntime+"=laptop-test") {
		t.Errorf("label missing: %s", argsString(args))
	}
	// Docker does not understand Kubernetes quantities.
	if !contains(args, "--memory", "2g") {
		t.Errorf("memory not converted: %s", argsString(args))
	}
	if !contains(args, "--cpus", "1") {
		t.Errorf("cpus missing: %s", argsString(args))
	}
}

// A port with no host assignment must not be published: "-p :22" would make
// docker pick a random port, and the address in status would be a lie.
func TestRunArgsSkipsUnassignedPorts(t *testing.T) {
	args := RunArgs(runtimesvc.Container{
		Name:  "x",
		Image: "img",
		Ports: []runtimesvc.Port{{Name: "ssh", Container: 22, Host: 0}},
	}, "127.0.0.1")

	if contains(args, "--publish") {
		t.Errorf("published a port with no host assignment: %s", argsString(args))
	}
}

func TestRunArgsIsDeterministic(t *testing.T) {
	c := runtimesvc.Container{
		Name:  "x",
		Image: "img",
		Env:   map[string]string{"B": "2", "A": "1", "C": "3"},
		Labels: map[string]string{
			runtimesvc.LabelRuntime: "x", runtimesvc.LabelLocation: "l", runtimesvc.LabelSpecHash: "h",
		},
		Ports: []runtimesvc.Port{
			{Name: "b", Container: 2, Host: 20}, {Name: "a", Container: 1, Host: 10},
		},
	}
	first := argsString(RunArgs(c, "127.0.0.1"))
	for i := 0; i < 20; i++ {
		if got := argsString(RunArgs(c, "127.0.0.1")); got != first {
			t.Fatalf("argument order varies between calls:\n%s\n%s", first, got)
		}
	}
}

func TestDockerMemory(t *testing.T) {
	cases := map[string]string{
		"2Gi": "2g", "512Mi": "512m", "1G": "1g", "256M": "256m",
		"1024": "1024", "": "", "  2Gi  ": "2g",
	}
	for in, want := range cases {
		if got := dockerMemory(in); got != want {
			t.Errorf("dockerMemory(%q) = %q, want %q", in, got, want)
		}
	}
}

// Port assignments are read back from our own labels, because the label
// carries the port's name — which is what templates and status speak in.
func TestParseInspect(t *testing.T) {
	out := `{
		"Id": "abc123",
		"Name": "/laptop-test",
		"State": {"Status": "running", "Running": true},
		"Config": {
			"Image": "registry/devcontainer:v1",
			"Labels": {
				"agent-platform.salt.x5.ru/runtime": "laptop-test",
				"agent-platform.salt.x5.ru/spec-hash": "sha256:deadbeef",
				"agent-platform.salt.x5.ru/port.ssh": "31002",
				"agent-platform.salt.x5.ru/port.api": "31001",
				"unrelated": "ignored"
			}
		}
	}`

	obs, err := parseInspect(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if obs.Name != "laptop-test" {
		t.Errorf("name = %q, want the leading slash stripped", obs.Name)
	}
	if obs.ID != "abc123" || obs.Image != "registry/devcontainer:v1" || !obs.Running {
		t.Errorf("got %+v", obs)
	}
	if obs.SpecHash() != "sha256:deadbeef" {
		t.Errorf("specHash = %q", obs.SpecHash())
	}
	if obs.Ports["ssh"] != 31002 || obs.Ports["api"] != 31001 {
		t.Errorf("ports = %v", obs.Ports)
	}
	if len(obs.Ports) != 2 {
		t.Errorf("unrelated labels leaked into ports: %v", obs.Ports)
	}
}

func TestParseInspectRejectsGarbage(t *testing.T) {
	if _, err := parseInspect("not json"); err == nil {
		t.Fatal("expected an error")
	}
}

// docker's wording differs by object type and by version; treating it as an
// exact string once cost a debugging round.
func TestIsNotFound(t *testing.T) {
	yes := []string{
		"Error response from daemon: No such container: laptop-test",
		"Error: No such object: x",
		"error during connect: No such image: img",
	}
	for _, m := range yes {
		if !isNotFound(errString(m)) {
			t.Errorf("not recognised as absent: %s", m)
		}
	}
	if isNotFound(errString("permission denied while trying to connect to the Docker daemon")) {
		t.Error("a daemon failure was mistaken for an absent container")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// A host directory and a named volume are the same flag to docker, told apart
// by the leading slash. Getting the source wrong here produces a container that
// silently mounts a brand-new empty volume called "/Users/..." instead of the
// user's directory.
func TestRunArgsBindsHostDirectories(t *testing.T) {
	c := runtimesvc.Container{
		Name:  "box",
		Image: "claude-box:latest",
		Mounts: []runtimesvc.Mount{
			{Volume: "box-home", Path: "/home/dev"},
			{Host: "/Users/dev/code", Path: "/work/code"},
			{Host: "/Users/dev/docs", Path: "/work/docs", ReadOnly: true},
		},
	}

	args := RunArgs(c, "127.0.0.1")

	if !contains(args, "--volume", "/Users/dev/code:/work/code") {
		t.Errorf("host directory not bound: %s", argsString(args))
	}
	if !contains(args, "--volume", "/Users/dev/docs:/work/docs:ro") {
		t.Errorf("read-only host directory not bound: %s", argsString(args))
	}
	if !contains(args, "--volume", "box-home:/home/dev") {
		t.Errorf("the named volume was lost: %s", argsString(args))
	}
}
