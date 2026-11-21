// Package build rebuilds compose services whose base images changed.
//
// Three backends exist:
//   - compose: shells out to `docker compose build` (requires the docker CLI
//     with the compose plugin, and the project directory visible at the same
//     path as on the host, or a path_map).
//   - api: streams a tar of the build context to the Docker Engine ImageBuild
//     endpoint (classic builder, no BuildKit features).
//   - command: a user supplied command template (e.g. ssh to the host).
package build

import (
	"context"
	"log/slog"

	"github.com/dansimau/dockwarden/internal/compose"
)

// Request describes a service to rebuild.
type Request struct {
	Info compose.Info
	Spec *compose.BuildSpec
	// Tag is the image name the built image must carry (what the running
	// container uses).
	Tag     string
	Pull    bool
	NoCache bool
	// BaseImages that triggered the rebuild (informational, exposed to templates).
	BaseImages []string
	// Labels to attach to the built image (api backend only).
	Labels map[string]string
}

// Builder rebuilds a service image.
type Builder interface {
	Name() string
	Build(ctx context.Context, req Request) error
}

// Logger returns a non-nil logger.
func logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.New(slog.DiscardHandler)
	}
	return l
}
