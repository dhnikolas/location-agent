package runtime

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
	agentapi "scm.x5.ru/dis.cloud/core/location-agent/api"
)

func mountFor(name, host, container string) *agentapi.VolumeMount {
	vm := &agentapi.VolumeMount{}
	vm.Name = name
	vm.Spec.HostPath = host
	vm.Spec.ContainerPath = container
	return vm
}

func templateWithHome() *platform.RuntimeTemplate {
	claim := &platform.PVCSource{}
	return &platform.RuntimeTemplate{
		Spec: platform.RuntimeTemplateSpec{
			Container: platform.ContainerSpec{
				Image:        "claude-box:latest",
				VolumeMounts: []platform.VolumeMount{{Name: "home", MountPath: "/home/dev"}},
			},
			Volumes: []platform.Volume{{Name: "home", PersistentVolumeClaim: claim}},
		},
	}
}

func planWith(t *testing.T, mounts ...*agentapi.VolumeMount) (Container, error) {
	t.Helper()
	rt := &platform.Runtime{}
	rt.Name = "box"
	return Plan(Input{
		Runtime:    rt,
		Template:   templateWithHome(),
		Location:   "laptop",
		HostMounts: mounts,
	})
}

func TestHostMountReachesTheContainer(t *testing.T) {
	c, err := planWith(t, mountFor("code", "/Users/dev/code", "/work/code"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	var found bool
	for _, m := range c.Mounts {
		if m.Path == "/work/code" {
			found = true
			if m.Host != "/Users/dev/code" {
				t.Errorf("host = %q, want /Users/dev/code", m.Host)
			}
			if m.Volume != "" {
				t.Errorf("a host mount must not also name a volume, got %q", m.Volume)
			}
		}
	}
	if !found {
		t.Fatalf("the host directory is not on the container: %+v", c.Mounts)
	}
}

// The template's own storage must win. Docker would accept the overlap and
// mount the host directory over the box's home, which reads as a box that lost
// its files rather than as a mount in the wrong place.
func TestHostMountCannotShadowTheTemplate(t *testing.T) {
	_, err := planWith(t, mountFor("home", "/Users/dev/code", "/home/dev"))
	if err == nil {
		t.Fatal("expected the clash with the template's home to be refused")
	}
	if !strings.Contains(err.Error(), "/home/dev") {
		t.Errorf("the error does not say which path clashes: %v", err)
	}
}

func TestTwoHostMountsCannotShareAPath(t *testing.T) {
	_, err := planWith(t,
		mountFor("a", "/Users/dev/one", "/work/shared"),
		mountFor("b", "/Users/dev/two", "/work/shared"),
	)
	if err == nil {
		t.Fatal("expected two mounts on one path to be refused")
	}
}

// The hash decides whether the container is recreated. If the order of mounts
// could move it, a box with two of them would be recreated on every reconcile
// — and the only symptom is a box that keeps restarting.
func TestHostMountOrderDoesNotMoveTheHash(t *testing.T) {
	a := mountFor("alpha", "/Users/dev/one", "/work/one")
	b := mountFor("beta", "/Users/dev/two", "/work/two")

	first, err := planWith(t, a, b)
	if err != nil {
		t.Fatal(err)
	}
	second, err := planWith(t, b, a)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash() != second.Hash() {
		t.Errorf("hash depends on the order the mounts arrived in:\n %s\n %s",
			first.Hash(), second.Hash())
	}
}

// Adding one has to change the hash, or the runtime reconcile would see the
// container as already correct and never recreate it — the mount would simply
// never appear.
func TestAddingAHostMountChangesTheHash(t *testing.T) {
	without, err := planWith(t)
	if err != nil {
		t.Fatal(err)
	}
	with, err := planWith(t, mountFor("code", "/Users/dev/code", "/work/code"))
	if err != nil {
		t.Fatal(err)
	}
	if without.Hash() == with.Hash() {
		t.Error("the hash ignores host mounts, so the container would never be recreated")
	}
}

func TestMountValidation(t *testing.T) {
	cases := []struct {
		name      string
		host      string
		container string
	}{
		{"relative host path", "code", "/work/code"},
		{"relative container path", "/Users/dev/code", "work/code"},
		{"empty host path", "", "/work/code"},
		{"container root", "/Users/dev/code", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateMount(tc.host, tc.container); err == nil {
				t.Error("expected this to be refused")
			}
		})
	}

	if err := ValidateMount("/Users/dev/code", "/work/code"); err != nil {
		t.Errorf("a plain absolute pair was refused: %v", err)
	}
}

// A missing directory is refused rather than created: docker would make it,
// owned by root and empty, and a typo would look like lost files.
func TestCheckMountWantsAnExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := CheckMount(dir, "/work/code"); err != nil {
		t.Errorf("an existing directory was refused: %v", err)
	}
	if err := CheckMount(dir+"/nope", "/work/code"); err == nil {
		t.Error("expected a missing directory to be refused")
	}
}

// A socket is the one thing besides a directory worth binding: a box given the
// machine's docker talks to the socket itself. A plain file stays refused, for
// the reason it always was.
func TestCheckMountAllowsASocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if err := CheckMount(sock, "/var/run/docker.sock"); err != nil {
		t.Errorf("a socket was refused: %v", err)
	}

	file := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckMount(file, "/work/notes.txt"); err == nil {
		t.Error("a regular file was accepted")
	}
}

// The spec hash decides whether a container is recreated, so its shape is a
// compatibility surface: a field added to Mount or Container changes the hash
// of every container already running, and every box on every machine is
// recreated once on upgrade for a change none of them use.
//
// This pins the hash of a plain box. If it moves, the change is only correct
// when it was meant to affect existing containers.
func TestSpecHashIsStable(t *testing.T) {
	c, err := planWith(t)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "sha256:0de1096027bd671039606b862394edd4ac808bc8a457af8212a3b1e6ea1307b1"
	if c.Hash() != golden {
		t.Errorf("the spec hash of an unchanged box moved:\n got %s\nwant %s\n"+
			"every existing container would be recreated on upgrade", c.Hash(), golden)
	}
}
