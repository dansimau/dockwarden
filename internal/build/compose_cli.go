package build

import (
	"context"
	"log/slog"

	"github.com/dansimau/dockwarden/internal/compose"
	"github.com/dansimau/dockwarden/internal/execx"
)

// ComposeCLI builds via `docker compose build`.
type ComposeCLI struct {
	Runner  execx.Runner
	Logger  *slog.Logger
	PathMap compose.PathMapper
}

func (b *ComposeCLI) Name() string { return "compose" }

// Build implements Builder.
func (b *ComposeCLI) Build(ctx context.Context, req Request) error {
	args := composeGlobalArgs(&req.Info, b.PathMap)
	args = append(args, "build")
	if req.Pull {
		args = append(args, "--pull")
	}
	if req.NoCache {
		args = append(args, "--no-cache")
	}
	args = append(args, req.Info.Service)
	logger(b.Logger).Info("building via docker compose", "project", req.Info.Project, "service", req.Info.Service)
	_, err := b.Runner.Run(ctx, execx.Command{Name: "docker", Args: args, Dir: req.Info.LocalWorkingDir(b.PathMap)})
	return err
}

// composeGlobalArgs mirrors strategy.ComposeArgs (duplicated to avoid an
// import cycle).
func composeGlobalArgs(info *compose.Info, m compose.PathMapper) []string {
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

// ComposeAvailable reports whether `docker compose` works.
func ComposeAvailable(ctx context.Context, runner execx.Runner) bool {
	if !execx.LookPath("docker") {
		return false
	}
	_, err := runner.Run(ctx, execx.Command{Name: "docker", Args: []string{"compose", "version"}, Quiet: true})
	return err == nil
}
