package provision

import (
	"context"
	"io"
	"log/slog"
	"testing"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
	pa "scm.x5.ru/dis.cloud/core/provision-agent/api"
)

type fakeRepo struct {
	stored  *pa.Provision
	created int
	updated int
	deleted int
	getErr  error
}

func (f *fakeRepo) Provision(context.Context, string) (*pa.Provision, bool, error) {
	if f.getErr != nil {
		return nil, false, f.getErr
	}
	if f.stored == nil {
		return nil, false, nil
	}
	return f.stored, true, nil
}

func (f *fakeRepo) CreateProvision(_ context.Context, p *pa.Provision) error {
	f.created++
	f.stored = p
	return nil
}

func (f *fakeRepo) UpdateProvision(_ context.Context, p *pa.Provision) error {
	f.updated++
	f.stored = p
	return nil
}

func (f *fakeRepo) DeleteProvision(context.Context, string) error {
	f.deleted++
	f.stored = nil
	return nil
}

func newService(repo Repository) *Service {
	return New(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

var defaults = platform.ProvisionDefaults{
	SkillPaths: platform.SkillPaths{AgentsDir: "/home/dev/.claude/agents", SkillsDir: "/home/dev/.claude/skills"},
}

// The Provision must land in the runtime's own shard: that is the shard the
// agent inside the container watches. Ours would be invisible to it.
func TestEnsureCreatesInTheRuntimeShard(t *testing.T) {
	repo := &fakeRepo{}
	if err := newService(repo).Ensure(context.Background(), "laptop-test", defaults, nil); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if repo.created != 1 {
		t.Fatalf("created %d times", repo.created)
	}
	p := repo.stored
	if p.ShardID != "laptop-test" || p.Namespace != "laptop-test" || p.Name != "laptop-test" {
		t.Errorf("placed at shard=%q ns=%q name=%q", p.ShardID, p.Namespace, p.Name)
	}
	if p.Spec.RuntimeRef.Name != "laptop-test" {
		t.Errorf("runtimeRef = %q", p.Spec.RuntimeRef.Name)
	}
	if p.Spec.SkillPaths != skillPaths(defaults.SkillPaths) {
		t.Errorf("skillPaths = %+v", p.Spec.SkillPaths)
	}
}

// Reconciles run on a timer; rewriting an unchanged resource every tick would
// bump its version and churn the control plane for nothing.
func TestEnsureIsQuietWhenUnchanged(t *testing.T) {
	repo := &fakeRepo{}
	svc := newService(repo)
	ctx := context.Background()

	if err := svc.Ensure(ctx, "laptop-test", defaults, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := svc.Ensure(ctx, "laptop-test", defaults, nil); err != nil {
			t.Fatal(err)
		}
	}
	if repo.created != 1 || repo.updated != 0 {
		t.Errorf("created=%d updated=%d, want one create and no updates", repo.created, repo.updated)
	}
}

func TestEnsureUpdatesWhenDefaultsChange(t *testing.T) {
	repo := &fakeRepo{}
	svc := newService(repo)
	ctx := context.Background()

	if err := svc.Ensure(ctx, "laptop-test", defaults, nil); err != nil {
		t.Fatal(err)
	}
	changed := platform.ProvisionDefaults{SkillPaths: platform.SkillPaths{AgentsDir: "/other", SkillsDir: "/other"}}
	if err := svc.Ensure(ctx, "laptop-test", changed, nil); err != nil {
		t.Fatal(err)
	}
	if repo.updated != 1 {
		t.Errorf("updated %d times", repo.updated)
	}
	if repo.stored.Spec.SkillPaths != skillPaths(changed.SkillPaths) {
		t.Errorf("skillPaths = %+v", repo.stored.Spec.SkillPaths)
	}
}

// The heartbeat the runtime's own agent writes into status must survive our
// update, or the platform would see the runtime as dead every time a template
// changed.
func TestEnsurePreservesStatusOnUpdate(t *testing.T) {
	repo := &fakeRepo{stored: &pa.Provision{
		Spec:   pa.ProvisionSpec{RuntimeRef: pa.LocalObjectRef{Name: "laptop-test"}},
		Status: pa.ProvisionStatus{Phase: pa.PhaseReady, AgentInfo: pa.AgentInfo{LastHeartbeat: "now"}},
	}}

	if err := newService(repo).Ensure(context.Background(), "laptop-test", defaults, nil); err != nil {
		t.Fatal(err)
	}
	if repo.stored.Status.AgentInfo.LastHeartbeat != "now" || repo.stored.Status.Phase != pa.PhaseReady {
		t.Errorf("status was clobbered: %+v", repo.stored.Status)
	}
}

// A user's file replaces the template's default at the same path rather than
// producing two entries for one destination.
func TestEnsureMergesFilesByPath(t *testing.T) {
	repo := &fakeRepo{}
	withFiles := platform.ProvisionDefaults{
		Files: []platform.FileEntry{
			{Path: "/etc/a", Content: "from-template"},
			{Path: "/etc/b", Content: "from-template"},
		},
	}
	uc := &pa.UserConfig{Spec: pa.UserConfigSpec{Files: []pa.FileEntry{
		{Path: "/etc/b", Content: "from-user"},
		{Path: "/etc/c", Content: "from-user"},
	}}}

	if err := newService(repo).Ensure(context.Background(), "laptop-test", withFiles, uc); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, f := range repo.stored.Spec.Files {
		if _, dup := got[f.Path]; dup {
			t.Fatalf("path %q appears twice: %+v", f.Path, repo.stored.Spec.Files)
		}
		got[f.Path] = f.Content
	}
	want := map[string]string{"/etc/a": "from-template", "/etc/b": "from-user", "/etc/c": "from-user"}
	for path, content := range want {
		if got[path] != content {
			t.Errorf("%s = %q, want %q", path, got[path], content)
		}
	}
}

func TestRemove(t *testing.T) {
	repo := &fakeRepo{stored: &pa.Provision{}}
	if err := newService(repo).Remove(context.Background(), "laptop-test"); err != nil {
		t.Fatal(err)
	}
	if repo.deleted != 1 {
		t.Errorf("deleted %d times", repo.deleted)
	}
}

func TestEnsureRejectsEmptyName(t *testing.T) {
	if err := newService(&fakeRepo{}).Ensure(context.Background(), "", defaults, nil); err == nil {
		t.Fatal("expected an error for an empty runtime name")
	}
}
