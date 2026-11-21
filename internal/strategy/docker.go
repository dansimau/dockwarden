package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"

	"github.com/dansimau/dockwarden/internal/compose"
	"github.com/dansimau/dockwarden/internal/dockerapi"
	"github.com/dansimau/dockwarden/internal/execx"
)

// Docker recreates containers natively via the Engine API, or via
// `docker compose up -d` for compose-managed containers when the CLI is
// available.
type Docker struct {
	Client      dockerapi.Client
	Runner      execx.Runner
	Logger      *slog.Logger
	StopTimeout time.Duration
	// UseComposeCLI enables `docker compose up` for compose containers.
	UseComposeCLI bool
	PathMap       compose.PathMapper
}

func (d *Docker) Name() string       { return "docker" }
func (d *Docker) HandlesBuild() bool { return false }
func (d *Docker) HandlesPull() bool  { return false }

// Update implements Strategy.
func (d *Docker) Update(ctx context.Context, t *Target) error {
	if t.Compose != nil && d.UseComposeCLI && len(t.Compose.ConfigFiles) > 0 {
		return d.composeUp(ctx, t)
	}
	return d.Recreate(ctx, t)
}

func (d *Docker) composeUp(ctx context.Context, t *Target) error {
	args := ComposeArgs(t.Compose, d.PathMap)
	args = append(args, "up", "-d", "--no-deps", "--no-build", t.Compose.Service)
	d.Logger.Info("recreating via docker compose", "container", t.ContainerName, "project", t.Compose.Project, "service", t.Compose.Service)
	_, err := d.Runner.Run(ctx, execx.Command{Name: "docker", Args: args, Dir: t.Compose.LocalWorkingDir(d.PathMap)})
	return err
}

// ComposeArgs returns the global `docker compose` arguments (-p, -f, env
// files, project directory) for a project.
func ComposeArgs(info *compose.Info, m compose.PathMapper) []string {
	args := []string{"compose", "-p", info.Project}
	for _, f := range info.LocalConfigFiles(m) {
		args = append(args, "-f", f)
	}
	for _, f := range info.EnvFiles {
		args = append(args, "--env-file", m.Map(f))
	}
	if wd := info.LocalWorkingDir(m); wd != "" {
		args = append(args, "--project-directory", wd)
	}
	return args
}

// Recreate stops the container, creates a new one with the same
// configuration but the new image, starts it and removes the old one. If
// starting the new container fails the old one is restored.
func (d *Docker) Recreate(ctx context.Context, t *Target) error {
	old := t.Inspect
	if old.ContainerJSONBase == nil || old.Config == nil {
		var err error
		old, err = d.Client.ContainerInspect(ctx, t.ContainerID)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", t.ContainerName, err)
		}
	}
	name := strings.TrimPrefix(old.Name, "/")
	cfg, hostCfg, netCfg, extraNets := d.newConfig(old, t.Image)
	log := d.Logger.With("container", name)

	wasRunning := old.State != nil && old.State.Running
	if wasRunning {
		log.Info("stopping container")
		timeout := int(d.StopTimeout.Seconds())
		if err := d.Client.ContainerStop(ctx, old.ID, container.StopOptions{Timeout: &timeout}); err != nil {
			return fmt.Errorf("stop %s: %w", name, err)
		}
	}
	tmpName := fmt.Sprintf("%s-dockwarden-old-%d", name, time.Now().Unix())
	if err := d.Client.ContainerRename(ctx, old.ID, tmpName); err != nil {
		return fmt.Errorf("rename %s: %w", name, err)
	}
	restore := func(cause error) error {
		log.Warn("restoring previous container after failure", "error", cause)
		var errs []error
		if err := d.Client.ContainerRename(ctx, old.ID, name); err != nil {
			errs = append(errs, fmt.Errorf("rename back: %w", err))
		}
		if wasRunning {
			if err := d.Client.ContainerStart(ctx, old.ID, container.StartOptions{}); err != nil {
				errs = append(errs, fmt.Errorf("restart old container: %w", err))
			}
		}
		return errors.Join(append([]error{cause}, errs...)...)
	}

	log.Info("creating container", "image", t.Image)
	created, err := d.Client.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, name)
	if err != nil {
		return restore(fmt.Errorf("create %s: %w", name, err))
	}
	for netName, ep := range extraNets {
		if err := d.Client.NetworkConnect(ctx, netName, created.ID, ep); err != nil {
			_ = d.Client.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
			return restore(fmt.Errorf("connect %s to network %s: %w", name, netName, err))
		}
	}
	if wasRunning {
		log.Info("starting container", "id", short(created.ID))
		if err := d.Client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
			_ = d.Client.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
			return restore(fmt.Errorf("start %s: %w", name, err))
		}
	}
	if err := d.Client.ContainerRemove(ctx, old.ID, container.RemoveOptions{Force: true}); err != nil {
		log.Warn("failed to remove old container", "id", short(old.ID), "error", err)
	}
	t.ContainerID = created.ID
	return nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// newConfig derives the create parameters for the replacement container.
// Returns the config, host config, networking config for the first network
// and any extra networks that must be connected after creation.
func (d *Docker) newConfig(old container.InspectResponse, image string) (*container.Config, *container.HostConfig, *network.NetworkingConfig, map[string]*network.EndpointSettings) {
	cfg := *old.Config
	cfg.Image = image
	// Docker sets the hostname to the container ID when none was given; don't
	// carry that over.
	if cfg.Hostname != "" && strings.HasPrefix(old.ID, cfg.Hostname) {
		cfg.Hostname = ""
	}
	hostCfg := old.HostConfig
	if hostCfg == nil {
		hostCfg = &container.HostConfig{}
	}
	// Convert the first container ID in the network mode (container:<id>) is
	// left untouched; Docker resolves it.

	netCfg := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
	extra := map[string]*network.EndpointSettings{}
	if old.NetworkSettings != nil {
		first := true
		for name, ep := range old.NetworkSettings.Networks {
			clean := cleanEndpoint(ep, old.ID)
			if first {
				netCfg.EndpointsConfig[name] = clean
				first = false
			} else {
				extra[name] = clean
			}
		}
	}
	// Containers using host/none/container: network modes have no user
	// networks to reattach.
	mode := string(hostCfg.NetworkMode)
	if strings.HasPrefix(mode, "container:") || mode == "host" || mode == "none" {
		netCfg = &network.NetworkingConfig{}
		extra = nil
	}
	return &cfg, hostCfg, netCfg, extra
}

func cleanEndpoint(ep *network.EndpointSettings, oldID string) *network.EndpointSettings {
	if ep == nil {
		return &network.EndpointSettings{}
	}
	out := &network.EndpointSettings{
		IPAMConfig: ep.IPAMConfig,
		Links:      ep.Links,
		NetworkID:  ep.NetworkID,
		DriverOpts: ep.DriverOpts,
	}
	for _, a := range ep.Aliases {
		if strings.HasPrefix(oldID, a) {
			continue // the auto-added short-ID alias
		}
		out.Aliases = append(out.Aliases, a)
	}
	return out
}
