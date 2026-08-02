// Package provision keeps a runtime's Provision resource in step with the
// template and user config it was created from.
//
// Provision is the contract between this agent and the agent inside the
// container: it says where skills are installed and which files to
// materialise. Without it the runtime starts and then quietly cannot do half
// of what it was created for.
package provision

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/reconcile-kit/api/resource"

	platform "scm.x5.ru/dis.cloud/core/agent-platform-cloop/api"
	pa "scm.x5.ru/dis.cloud/core/provision-agent/api"
)

// Repository stores Provision resources.
//
// They live in the runtime's own shard rather than this agent's — that is the
// shard the agent inside the container watches — so every call names the
// runtime explicitly instead of relying on an ambient one.
type Repository interface {
	Provision(ctx context.Context, runtime string) (*pa.Provision, bool, error)
	CreateProvision(ctx context.Context, p *pa.Provision) error
	UpdateProvision(ctx context.Context, p *pa.Provision) error
	DeleteProvision(ctx context.Context, runtime string) error
}

type Service struct {
	repo Repository
	log  *slog.Logger
}

func New(repo Repository, log *slog.Logger) *Service {
	return &Service{repo: repo, log: log}
}

// Ensure creates or updates the Provision for a runtime.
func (s *Service) Ensure(ctx context.Context, runtime string, defaults platform.ProvisionDefaults, userConfig *pa.UserConfig) error {
	if runtime == "" {
		return fmt.Errorf("runtime name is empty")
	}
	desired := pa.ProvisionSpec{
		RuntimeRef: pa.LocalObjectRef{Name: runtime},
		SkillPaths: skillPaths(defaults.SkillPaths),
		Files:      mergeFiles(templateFiles(defaults.Files), filesOf(userConfig)),
	}

	current, found, err := s.repo.Provision(ctx, runtime)
	if err != nil {
		return err
	}

	if !found {
		p := &pa.Provision{
			Resource: resource.Resource{
				Name:      runtime,
				Namespace: runtime,
				ShardID:   runtime,
			},
			Spec: desired,
		}
		if err := s.repo.CreateProvision(ctx, p); err != nil {
			return fmt.Errorf("create provision for %q: %w", runtime, err)
		}
		s.log.Info("created provision", "runtime", runtime)
		return nil
	}

	if specEqual(current.Spec, desired) {
		return nil
	}

	// The object read back is written back, so the status the runtime's own
	// agent maintains — its heartbeat — survives the update.
	current.Spec = desired
	if err := s.repo.UpdateProvision(ctx, current); err != nil {
		return fmt.Errorf("update provision for %q: %w", runtime, err)
	}
	s.log.Info("updated provision", "runtime", runtime)
	return nil
}

// Remove deletes the runtime's Provision. A missing one is success: the goal
// is that it is gone.
func (s *Service) Remove(ctx context.Context, runtime string) error {
	return s.repo.DeleteProvision(ctx, runtime)
}

// The template and the Provision are owned by different repositories, so the
// same JSON is two Go types. Converting explicitly is the price of not
// restating either schema — and it is a price the compiler collects, unlike a
// silently diverging copy.
func skillPaths(in platform.SkillPaths) pa.SkillPaths {
	return pa.SkillPaths{AgentsDir: in.AgentsDir, SkillsDir: in.SkillsDir}
}

func templateFiles(in []platform.FileEntry) []pa.FileEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]pa.FileEntry, 0, len(in))
	for _, f := range in {
		out = append(out, pa.FileEntry{Path: f.Path, Mode: f.Mode, Content: f.Content})
	}
	return out
}

func filesOf(uc *pa.UserConfig) []pa.FileEntry {
	if uc == nil {
		return nil
	}
	return uc.Spec.Files
}

// mergeFiles layers the user's files over the template's defaults, matching by
// path so a user can replace a default rather than end up with both.
func mergeFiles(defaults, user []pa.FileEntry) []pa.FileEntry {
	if len(user) == 0 {
		return defaults
	}
	out := make([]pa.FileEntry, 0, len(defaults)+len(user))
	index := map[string]int{}
	for _, f := range defaults {
		index[f.Path] = len(out)
		out = append(out, f)
	}
	for _, f := range user {
		if i, ok := index[f.Path]; ok {
			out[i] = f
			continue
		}
		index[f.Path] = len(out)
		out = append(out, f)
	}
	return out
}

func specEqual(a, b pa.ProvisionSpec) bool {
	if a.RuntimeRef != b.RuntimeRef || a.SkillPaths != b.SkillPaths || len(a.Files) != len(b.Files) {
		return false
	}
	for i := range a.Files {
		if a.Files[i] != b.Files[i] {
			return false
		}
	}
	return true
}
