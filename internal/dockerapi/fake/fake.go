// Package fake provides an in-memory dockerapi.Client for tests.
package fake

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Image is a fake local image.
type Image struct {
	ID          string
	RepoDigests []string
	RepoTags    []string
	Labels      map[string]string
}

// Client is a fake Docker client.
type Client struct {
	mu         sync.Mutex
	Containers map[string]container.InspectResponse // by ID
	Images     map[string]Image                     // by ID and by tag
	// PullHook is invoked on ImagePull; it may add images. Return an error to fail the pull.
	PullHook func(ref string) error
	// BuildHook is invoked on ImageBuild with the options; return error to fail.
	BuildHook func(opts build.ImageBuildOptions) error
	// Calls records method invocations in order, e.g. "ContainerStop web".
	Calls []string
	// Fail maps a method name to an error to return.
	Fail map[string]error
	// PullAuth records the RegistryAuth passed to ImagePull by ref.
	PullAuth map[string]string
	nextID   int
}

// New creates an empty fake client.
func New() *Client {
	return &Client{
		Containers: map[string]container.InspectResponse{},
		Images:     map[string]Image{},
		Fail:       map[string]error{},
		PullAuth:   map[string]string{},
	}
}

func (c *Client) record(s string) { c.Calls = append(c.Calls, s) }

// CallsMatching returns recorded calls with the given prefix.
func (c *Client) CallsMatching(prefix string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.Calls {
		if strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}
	return out
}

// AddImage registers an image under its ID and tags.
func (c *Client) AddImage(img Image) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Images[img.ID] = img
	for _, t := range img.RepoTags {
		c.Images[t] = img
	}
}

// AddContainer registers a running container.
func (c *Client) AddContainer(id, name, imageRef, imageID string, labels map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if labels == nil {
		labels = map[string]string{}
	}
	c.Containers[id] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:         id,
			Name:       "/" + name,
			Image:      imageID,
			State:      &container.State{Running: true, Status: "running"},
			HostConfig: &container.HostConfig{},
		},
		Config:          &container.Config{Image: imageRef, Labels: labels, Hostname: name},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{"bridge": {NetworkID: "bridge-id"}}},
	}
}

func (c *Client) Ping(ctx context.Context) (types.Ping, error) {
	return types.Ping{APIVersion: "1.47"}, c.Fail["Ping"]
}

func (c *Client) ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Fail["ContainerList"]; err != nil {
		return nil, err
	}
	var out []container.Summary
	for _, ct := range c.Containers {
		if !options.All && !ct.State.Running {
			continue
		}
		out = append(out, container.Summary{ID: ct.ID, Names: []string{ct.Name}, Image: ct.Config.Image, ImageID: ct.Image, Labels: ct.Config.Labels, State: ct.State.Status})
	}
	return out, nil
}

func (c *Client) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Fail["ContainerInspect"]; err != nil {
		return container.InspectResponse{}, err
	}
	for _, ct := range c.Containers {
		if ct.ID == id || ct.Name == "/"+id || ct.Name == id {
			return ct, nil
		}
	}
	return container.InspectResponse{}, fmt.Errorf("No such container: %s", id)
}

func (c *Client) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("ContainerCreate " + name + " " + config.Image)
	if err := c.Fail["ContainerCreate"]; err != nil {
		return container.CreateResponse{}, err
	}
	for _, ct := range c.Containers {
		if ct.Name == "/"+name {
			return container.CreateResponse{}, fmt.Errorf("Conflict. The container name %q is already in use", name)
		}
	}
	img, ok := c.Images[config.Image]
	if !ok {
		return container.CreateResponse{}, fmt.Errorf("No such image: %s", config.Image)
	}
	c.nextID++
	id := fmt.Sprintf("new%d", c.nextID)
	nets := map[string]*network.EndpointSettings{}
	if networkingConfig != nil {
		for k, v := range networkingConfig.EndpointsConfig {
			nets[k] = v
		}
	}
	c.Containers[id] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: id, Name: "/" + name, Image: img.ID, State: &container.State{Status: "created"}, HostConfig: hostConfig},
		Config:            config,
		NetworkSettings:   &container.NetworkSettings{Networks: nets},
	}
	return container.CreateResponse{ID: id}, nil
}

func (c *Client) ContainerStart(ctx context.Context, id string, options container.StartOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("ContainerStart " + id)
	if err := c.Fail["ContainerStart"]; err != nil {
		return err
	}
	ct, ok := c.Containers[id]
	if !ok {
		return fmt.Errorf("No such container: %s", id)
	}
	ct.State.Running = true
	ct.State.Status = "running"
	return nil
}

func (c *Client) ContainerStop(ctx context.Context, id string, options container.StopOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("ContainerStop " + id)
	if err := c.Fail["ContainerStop"]; err != nil {
		return err
	}
	ct, ok := c.Containers[id]
	if !ok {
		return fmt.Errorf("No such container: %s", id)
	}
	ct.State.Running = false
	ct.State.Status = "exited"
	return nil
}

func (c *Client) ContainerRemove(ctx context.Context, id string, options container.RemoveOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("ContainerRemove " + id)
	if err := c.Fail["ContainerRemove"]; err != nil {
		return err
	}
	if _, ok := c.Containers[id]; !ok {
		return fmt.Errorf("No such container: %s", id)
	}
	delete(c.Containers, id)
	return nil
}

func (c *Client) ContainerRename(ctx context.Context, id, newName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("ContainerRename " + id + " " + newName)
	if err := c.Fail["ContainerRename"]; err != nil {
		return err
	}
	ct, ok := c.Containers[id]
	if !ok {
		return fmt.Errorf("No such container: %s", id)
	}
	ct.Name = "/" + newName
	c.Containers[id] = ct
	return nil
}

func (c *Client) NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("NetworkConnect " + networkID + " " + containerID)
	return c.Fail["NetworkConnect"]
}

func (c *Client) ImageInspect(ctx context.Context, id string, opts ...client.ImageInspectOption) (image.InspectResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Fail["ImageInspect"]; err != nil {
		return image.InspectResponse{}, err
	}
	img, ok := c.Images[id]
	if !ok {
		// allow lookup without :latest
		img, ok = c.Images[id+":latest"]
	}
	if !ok {
		return image.InspectResponse{}, fmt.Errorf("No such image: %s", id)
	}
	return image.InspectResponse{ID: img.ID, RepoDigests: img.RepoDigests, RepoTags: img.RepoTags, Config: &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{Labels: img.Labels}}}, nil
}

func (c *Client) ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error) {
	c.mu.Lock()
	c.record("ImagePull " + ref)
	c.PullAuth[ref] = options.RegistryAuth
	hook := c.PullHook
	c.mu.Unlock()
	if err := c.Fail["ImagePull"]; err != nil {
		return nil, err
	}
	if hook != nil {
		if err := hook(ref); err != nil {
			return io.NopCloser(strings.NewReader(`{"error":"` + err.Error() + `"}`)), nil
		}
	}
	return io.NopCloser(strings.NewReader(`{"status":"Pulling from x"}` + "\n" + `{"status":"Status: Downloaded newer image"}` + "\n")), nil
}

func (c *Client) ImageBuild(ctx context.Context, buildContext io.Reader, options build.ImageBuildOptions) (build.ImageBuildResponse, error) {
	c.mu.Lock()
	c.record("ImageBuild " + strings.Join(options.Tags, ","))
	hook := c.BuildHook
	c.mu.Unlock()
	if err := c.Fail["ImageBuild"]; err != nil {
		return build.ImageBuildResponse{}, err
	}
	// consume the context so tar writers don't block
	io.Copy(io.Discard, buildContext) //nolint:errcheck
	if hook != nil {
		if err := hook(options); err != nil {
			return build.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(`{"errorDetail":{"message":"` + err.Error() + `"},"error":"` + err.Error() + `"}`))}, nil
		}
	}
	return build.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(`{"stream":"Step 1/1 : FROM x\n"}` + "\n" + `{"aux":{"ID":"sha256:built"}}` + "\n"))}, nil
}

func (c *Client) ImageRemove(ctx context.Context, id string, options image.RemoveOptions) ([]image.DeleteResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("ImageRemove " + id)
	if err := c.Fail["ImageRemove"]; err != nil {
		return nil, err
	}
	delete(c.Images, id)
	return []image.DeleteResponse{{Deleted: id}}, nil
}
