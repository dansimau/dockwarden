package strategy

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/network"

	"github.com/dansimau/dockwarden/internal/compose"
	"github.com/dansimau/dockwarden/internal/dockerapi/fake"
	"github.com/dansimau/dockwarden/internal/execx"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestCommandRender(t *testing.T) {
	runner := &execx.Fake{}
	c, err := NewCommand(`cd {{.WorkingDir | shellquote}} && docker compose up -d {{.Service}} # {{.Kind}} {{.ContainerName}} {{.Image}} {{.Labels.foo}}`, "/bin/sh", true, false, time.Minute, runner, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if !c.HandlesBuild() || c.HandlesPull() {
		t.Fatal("flags")
	}
	fc := fake.New()
	fc.AddContainer("abc", "web", "img:1", "sha256:1", map[string]string{"foo": "bar"})
	insp, _ := fc.ContainerInspect(context.Background(), "abc")
	target := &Target{Kind: KindRebuild, ContainerID: "abc", ContainerName: "web", Image: "img:1", Compose: &compose.Info{Project: "p", Service: "web", WorkingDir: "/srv/it's"}, Inspect: insp}
	if err := c.Update(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if len(runner.Commands) != 1 {
		t.Fatalf("commands %v", runner.Commands)
	}
	got := runner.Commands[0].Args[1]
	want := `cd '/srv/it'\''s' && docker compose up -d web # rebuild web img:1 bar`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	runner.Handler = func(cmd execx.Command) (string, error) { return "", errors.New("boom") }
	if err := c.Update(context.Background(), target); err == nil {
		t.Fatal("expected error")
	}
}

func TestCommandBadTemplate(t *testing.T) {
	if _, err := NewCommand("{{.Nope", "", false, false, 0, &execx.Fake{}, testLogger()); err == nil {
		t.Fatal("expected parse error")
	}
}

func newDockerStrategy(fc *fake.Client, runner *execx.Fake) *Docker {
	return &Docker{Client: fc, Runner: runner, Logger: testLogger(), StopTimeout: 10 * time.Second}
}

func TestDockerRecreate(t *testing.T) {
	fc := fake.New()
	fc.AddImage(fake.Image{ID: "sha256:new", RepoTags: []string{"nginx:1"}})
	fc.AddContainer("abcdef123456789", "web", "nginx:1", "sha256:old", nil)
	ct := fc.Containers["abcdef123456789"]
	ct.Config.Hostname = "abcdef123456" // auto hostname
	ct.NetworkSettings.Networks["other"] = &network.EndpointSettings{Aliases: []string{"abcdef123456", "web"}}
	fc.Containers["abcdef123456789"] = ct

	d := newDockerStrategy(fc, &execx.Fake{})
	target := &Target{Kind: KindImage, ContainerID: "abcdef123456789", ContainerName: "web", Image: "nginx:1", Inspect: ct}
	if err := d.Update(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(fc.Calls, "\n")
	for _, want := range []string{"ContainerStop abcdef123456789", "ContainerRename abcdef123456789 web-dockwarden-old-", "ContainerCreate web nginx:1", "NetworkConnect", "ContainerStart new1", "ContainerRemove abcdef123456789"} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing call %q in:\n%s", want, calls)
		}
	}
	nc := fc.Containers["new1"]
	if nc.Config.Hostname != "" || nc.Image != "sha256:new" || !nc.State.Running {
		t.Fatalf("new container %+v", nc.ContainerJSONBase)
	}
	if target.ContainerID != "new1" {
		t.Fatal("target ID not updated")
	}
	if _, ok := fc.Containers["abcdef123456789"]; ok {
		t.Fatal("old container should be removed")
	}
}

func TestDockerRecreateRollback(t *testing.T) {
	fc := fake.New()
	fc.AddImage(fake.Image{ID: "sha256:new", RepoTags: []string{"nginx:1"}})
	fc.AddContainer("old1", "web", "nginx:1", "sha256:old", nil)
	fc.Fail["ContainerCreate"] = errors.New("create failed")
	d := newDockerStrategy(fc, &execx.Fake{})
	err := d.Update(context.Background(), &Target{ContainerID: "old1", ContainerName: "web", Image: "nginx:1", Inspect: fc.Containers["old1"]})
	if err == nil || !strings.Contains(err.Error(), "create failed") {
		t.Fatalf("expected create error, got %v", err)
	}
	old := fc.Containers["old1"]
	if old.Name != "/web" || !old.State.Running {
		t.Fatalf("old container not restored: %+v", old.ContainerJSONBase)
	}
}

func TestDockerComposeUp(t *testing.T) {
	fc := fake.New()
	runner := &execx.Fake{}
	d := newDockerStrategy(fc, runner)
	d.UseComposeCLI = true
	d.PathMap = compose.PathMapper{"/volume1/docker": "/projects"}
	info := &compose.Info{Project: "p", Service: "web", WorkingDir: "/volume1/docker/p", ConfigFiles: []string{"/volume1/docker/p/compose.yml"}, EnvFiles: []string{"/volume1/docker/p/.env.prod"}}
	if err := d.Update(context.Background(), &Target{ContainerID: "x", ContainerName: "p-web-1", Image: "p-web", Compose: info}); err != nil {
		t.Fatal(err)
	}
	got := runner.Strings()[0]
	want := "docker compose -p p -f /projects/p/compose.yml --env-file /projects/p/.env.prod --project-directory /projects/p up -d --no-deps --no-build web"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if runner.Commands[0].Dir != "/projects/p" {
		t.Fatalf("dir %s", runner.Commands[0].Dir)
	}
	if len(fc.Calls) != 0 {
		t.Fatal("should not touch API")
	}
}

func TestNewConfigHostNetwork(t *testing.T) {
	fc := fake.New()
	fc.AddContainer("c1", "h", "img", "sha", nil)
	ct := fc.Containers["c1"]
	ct.HostConfig.NetworkMode = "host"
	d := newDockerStrategy(fc, nil)
	_, _, netCfg, extra := d.newConfig(ct, "img")
	if len(netCfg.EndpointsConfig) != 0 || extra != nil {
		t.Fatal("host network should not carry endpoints")
	}
}
