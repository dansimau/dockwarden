package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/dansimau/dockwarden/internal/synology"
)

// SynologyClient is the subset of *synology.Client used by the strategy.
type SynologyClient interface {
	Login(ctx context.Context) error
	Logout(ctx context.Context) error
	LoggedIn() bool
	ListProjects(ctx context.Context) ([]synology.Project, error)
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
type Synology struct {
	Client     SynologyClient
	Logger     *slog.Logger
	ProjectMap map[string]string

	projects []synology.Project
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
	p, err := s.findProject(ctx, t)
	if err != nil {
		return err
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

// findProject maps a compose project to a DSM project: explicit project_map
// entry, then matching name, then matching path.
func (s *Synology) findProject(ctx context.Context, t *Target) (*synology.Project, error) {
	if s.projects == nil {
		ps, err := s.Client.ListProjects(ctx)
		if err != nil {
			return nil, fmt.Errorf("list DSM projects: %w", err)
		}
		s.projects = ps
	}
	name := t.Compose.Project
	if mapped, ok := s.ProjectMap[name]; ok {
		name = mapped
	}
	wd := filepath.Clean(t.Compose.WorkingDir)
	var byPath *synology.Project
	for i := range s.projects {
		p := &s.projects[i]
		if strings.EqualFold(p.Name, name) {
			return p, nil
		}
		if wd != "" && wd != "." && filepath.Clean(p.Path) == wd {
			byPath = p
		}
	}
	if byPath != nil {
		return byPath, nil
	}
	return nil, fmt.Errorf("no DSM Container Manager project matches compose project %q (dir %s); set strategy.synology.project_map", t.Compose.Project, t.Compose.WorkingDir)
}
