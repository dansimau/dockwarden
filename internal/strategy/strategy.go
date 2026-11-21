// Package strategy defines how updated containers are restarted or
// recreated. Strategies are pluggable: the built-in ones are "docker"
// (native recreate / docker compose up), "command" (user template) and
// "synology" (Container Manager Web API).
package strategy

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types/container"

	"github.com/dansimau/dockwarden/internal/compose"
)

// Kind is the type of update that triggered the restart.
type Kind string

const (
	// KindImage means the container's image tag has a newer digest and was pulled.
	KindImage Kind = "image"
	// KindRebuild means a compose service built from a local Dockerfile had a
	// newer base image and was (or must be) rebuilt.
	KindRebuild Kind = "rebuild"
)

// Target describes a container that needs to be restarted/recreated.
type Target struct {
	Kind          Kind
	ContainerID   string
	ContainerName string
	// Image is the image reference (tag) the container should run.
	Image string
	// OldImageID is the image ID the container was running before the update.
	OldImageID string
	// Compose is set for compose-managed containers.
	Compose *compose.Info
	// Build is set for rebuild targets.
	Build *compose.BuildSpec
	// BaseImages lists the base images that changed (rebuild targets).
	BaseImages []string
	// Inspect is the full container inspection, used by the docker strategy.
	Inspect container.InspectResponse
}

// ShortID returns the first 12 characters of the container ID.
func (t *Target) ShortID() string {
	if len(t.ContainerID) > 12 {
		return t.ContainerID[:12]
	}
	return t.ContainerID
}

// Project returns the compose project name or "".
func (t *Target) Project() string {
	if t.Compose == nil {
		return ""
	}
	return t.Compose.Project
}

// Service returns the compose service name or "".
func (t *Target) Service() string {
	if t.Compose == nil {
		return ""
	}
	return t.Compose.Service
}

// WorkingDir returns the compose project directory (host path) or "".
func (t *Target) WorkingDir() string {
	if t.Compose == nil {
		return ""
	}
	return t.Compose.WorkingDir
}

// ConfigFiles returns the compose config files joined by commas.
func (t *Target) ConfigFiles() string {
	if t.Compose == nil {
		return ""
	}
	return strings.Join(t.Compose.ConfigFiles, ",")
}

// Strategy applies an update to a container.
type Strategy interface {
	// Name identifies the strategy in logs.
	Name() string
	// HandlesBuild reports whether the strategy rebuilds images itself, so
	// the engine should skip its own build step for rebuild targets.
	HandlesBuild() bool
	// HandlesPull reports whether the strategy pulls images itself.
	HandlesPull() bool
	// Update restarts/recreates the target so it runs the new image.
	Update(ctx context.Context, t *Target) error
}
