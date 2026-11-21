// Package dockerapi defines the subset of the Docker Engine API client used
// by dockwarden, so it can be replaced by a fake in tests.
package dockerapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Client is the subset of *client.Client that dockwarden uses.
type Client interface {
	Ping(ctx context.Context) (types.Ping, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, id string, options container.StartOptions) error
	ContainerStop(ctx context.Context, id string, options container.StopOptions) error
	ContainerRemove(ctx context.Context, id string, options container.RemoveOptions) error
	ContainerRename(ctx context.Context, id, newName string) error
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	ImageInspect(ctx context.Context, id string, opts ...client.ImageInspectOption) (image.InspectResponse, error)
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options build.ImageBuildOptions) (build.ImageBuildResponse, error)
	ImageRemove(ctx context.Context, id string, options image.RemoveOptions) ([]image.DeleteResponse, error)
}

var _ Client = (*client.Client)(nil)

// New connects to the Docker daemon. host overrides DOCKER_HOST when set.
func New(host string) (*client.Client, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	return client.NewClientWithOpts(opts...)
}

// JSONMessage is a line of the JSON stream returned by pull/build.
type JSONMessage struct {
	Stream      string `json:"stream"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
	Aux json.RawMessage `json:"aux"`
}

// DrainJSONStream consumes a Docker JSON message stream, calling onMessage
// for every message, and returns an error if the stream reports one.
func DrainJSONStream(r io.Reader, onMessage func(JSONMessage)) error {
	dec := json.NewDecoder(r)
	for {
		var m JSONMessage
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode docker stream: %w", err)
		}
		if onMessage != nil {
			onMessage(m)
		}
		if m.Error != "" {
			return fmt.Errorf("%s", strings.TrimSpace(m.Error))
		}
		if m.ErrorDetail != nil && m.ErrorDetail.Message != "" {
			return fmt.Errorf("%s", strings.TrimSpace(m.ErrorDetail.Message))
		}
	}
}
