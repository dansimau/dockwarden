package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dansimau/dockwarden/internal/compose"
	"github.com/dansimau/dockwarden/internal/synology"
)

// SynologyClient is the subset of *synology.Client used by the strategy.
type SynologyClient interface {
	Login(ctx context.Context) error
	Logout(ctx context.Context) error
	LoggedIn() bool
	ListProjects(ctx context.Context) ([]synology.Project, error)
	GetProject(ctx context.Context, id string) (*synology.Project, error)
	ProjectAction(ctx context.Context, method, id string) error
	ContainerAction(ctx context.Context, method, name string) error
	ContainerDelete(ctx context.Context, name string, preserveProfile, force bool) error
}

// Synology redeploys through DSM Container Manager's Web API so that DSM
// stays the owner of projects and containers.
//
// Compose projects: build_stream (docker compose up --build --no-start, as
// the Container Manager "Build" button does) followed by start_stream.
// Plain containers: stop, delete with preserve_profile, start — DSM then
// recreates the container from its saved profile using the freshly pulled
// image.
//
// Compose projects are mapped to DSM projects automatically: for every DSM
// project the compose project name is derived from its compose file
// (`name:`), its .env (COMPOSE_PROJECT_NAME, when the project directory is
// readable) or its directory name, exactly as docker compose does.
// ProjectMap overrides that for edge cases.
type Synology struct {
	Client SynologyClient
	Logger *slog.Logger
	// ProjectMap maps compose project names to DSM project names (or IDs),
	// overriding the automatic mapping.
	ProjectMap map[string]string
	// PathMap translates DSM project paths to local paths so .env files can
	// be read; nil means paths are identical.
	PathMap compose.PathMapper

	// per-run caches
	projects  []synology.Project
	byCompose map[string]*synology.Project
	// done tracks projects already redeployed in this run so that several
	// updated containers of one project trigger a single build+start.
	done map[string]bool
}

func (s *Synology) Name() string       { return "synology" }
func (s *Synology) HandlesBuild() bool { return true }
func (s *Synology) HandlesPull() bool  { return false }

func (s *Synology) ensureLogin(ctx context.Context) error {
	if s.Client.LoggedIn() {
		return nil
	}
	return s.Client.Login(ctx)
}

// Close logs out of DSM.
func (s *Synology) Close(ctx context.Context) error {
	if s.Client.LoggedIn() {
		return s.Client.Logout(ctx)
	}
	return nil
}

// BeginRun resets per-run caches so a fresh project list is fetched.
func (s *Synology) BeginRun(ctx context.Context) error {
	s.projects, s.byCompose, s.done = nil, nil, nil
	return nil
}

// EndRun logs out of DSM.
func (s *Synology) EndRun(ctx context.Context) error {
	return s.Close(ctx)
}

// Update implements Strategy.
func (s *Synology) Update(ctx context.Context, t *Target) error {
	if err := s.ensureLogin(ctx); err != nil {
		return err
	}
	if t.Compose != nil {
		return s.updateProject(ctx, t)
	}
	return s.updateContainer(ctx, t)
}

func (s *Synology) updateProject(ctx context.Context, t *Target) error {
	p, err := s.FindProject(ctx, t.Compose)
	if err != nil {
		return err
	}
	if s.done[p.ID] {
		s.Logger.Debug("project already redeployed in this run", "project", p.Name)
		return nil
	}
	log := s.Logger.With("project", p.Name, "id", p.ID, "service", t.Compose.Service)
	log.Info("building project via DSM Container Manager")
	if err := s.Client.ProjectAction(ctx, synology.ProjectBuild, p.ID); err != nil {
		return fmt.Errorf("build project %s: %w", p.Name, err)
	}
	log.Info("starting project via DSM Container Manager")
	if err := s.Client.ProjectAction(ctx, synology.ProjectStart, p.ID); err != nil {
		return fmt.Errorf("start project %s: %w", p.Name, err)
	}
	if s.done == nil {
		s.done = map[string]bool{}
	}
	s.done[p.ID] = true
	return nil
}

func (s *Synology) updateContainer(ctx context.Context, t *Target) error {
	name := t.ContainerName
	log := s.Logger.With("container", name)
	log.Info("recreating container via DSM Container Manager")
	if err := s.Client.ContainerAction(ctx, synology.ContainerStop, name); err != nil {
		return fmt.Errorf("stop %s: %w", name, err)
	}
	if err := s.Client.ContainerDelete(ctx, name, true, false); err != nil {
		return fmt.Errorf("delete %s (preserving profile): %w", name, err)
	}
	if err := s.Client.ContainerAction(ctx, synology.ContainerStart, name); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return nil
}

// Mapping is one entry of the automatic compose -> DSM project mapping.
type Mapping struct {
	ComposeName string
	Project     synology.Project
}

// Mappings lists the automatically derived compose project name for every
// DSM project, sorted by compose name. Requires a login.
func (s *Synology) Mappings(ctx context.Context) ([]Mapping, error) {
	if err := s.loadProjects(ctx); err != nil {
		return nil, err
	}
	out := make([]Mapping, 0, len(s.byCompose))
	for name, p := range s.byCompose {
		out = append(out, Mapping{ComposeName: name, Project: *p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ComposeName < out[j].ComposeName })
	return out, nil
}

// loadProjects fetches all DSM projects (with their compose content) once
// per run and derives their compose project names.
func (s *Synology) loadProjects(ctx context.Context) error {
	if s.byCompose != nil {
		return nil
	}
	ps, err := s.Client.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("list DSM projects: %w", err)
	}
	s.byCompose = map[string]*synology.Project{}
	for i := range ps {
		p := &ps[i]
		if p.Content == "" {
			full, err := s.Client.GetProject(ctx, p.ID)
			if err != nil {
				s.Logger.Warn("cannot fetch DSM project details; deriving compose name from path only", "project", p.Name, "error", err)
			} else {
				*p = *full
			}
		}
		envFile := ""
		if p.Path != "" {
			envFile = filepath.Join(s.PathMap.Map(p.Path), ".env")
		}
		name := p.ComposeName(envFile)
		s.Logger.Debug("mapped DSM project", "project", p.Name, "path", p.Path, "compose_name", name)
		if other, dup := s.byCompose[name]; dup {
			s.Logger.Warn("two DSM projects derive the same compose project name; use strategy.synology.project_map", "compose_name", name, "projects", []string{other.Name, p.Name})
			continue
		}
		s.byCompose[name] = p
	}
	s.projects = ps
	return nil
}

// FindProject maps a compose project to a DSM project: an explicit
// ProjectMap entry (by DSM name or ID), then the automatically derived
// compose name, then a DSM project with the same name, then the same path.
func (s *Synology) FindProject(ctx context.Context, info *compose.Info) (*synology.Project, error) {
	if err := s.loadProjects(ctx); err != nil {
		return nil, err
	}
	if mapped, ok := s.ProjectMap[info.Project]; ok {
		for i := range s.projects {
			p := &s.projects[i]
			if strings.EqualFold(p.Name, mapped) || p.ID == mapped {
				return p, nil
			}
		}
		return nil, fmt.Errorf("strategy.synology.project_map maps %q to %q, but no DSM project has that name or id", info.Project, mapped)
	}
	if p, ok := s.byCompose[compose.NormalizeProjectName(info.Project)]; ok {
		return p, nil
	}
	wd := filepath.Clean(info.WorkingDir)
	var byPath *synology.Project
	for i := range s.projects {
		p := &s.projects[i]
		if strings.EqualFold(p.Name, info.Project) {
			return p, nil
		}
		if wd != "" && wd != "." && filepath.Clean(p.Path) == wd {
			byPath = p
		}
	}
	if byPath != nil {
		return byPath, nil
	}
	return nil, fmt.Errorf("no DSM Container Manager project matches compose project %q (dir %s); run `dockwarden synology projects` and set strategy.synology.project_map", info.Project, info.WorkingDir)
}
