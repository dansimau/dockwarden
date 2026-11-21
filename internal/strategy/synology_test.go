package strategy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dansimau/dockwarden/internal/compose"
	"github.com/dansimau/dockwarden/internal/synology"
)

type fakeSyno struct {
	loggedIn bool
	projects []synology.Project
	calls    []string
	fail     map[string]error
}

func (f *fakeSyno) Login(ctx context.Context) error {
	f.calls = append(f.calls, "login")
	f.loggedIn = true
	return f.fail["login"]
}
func (f *fakeSyno) Logout(ctx context.Context) error {
	f.calls = append(f.calls, "logout")
	f.loggedIn = false
	return nil
}
func (f *fakeSyno) LoggedIn() bool { return f.loggedIn }
func (f *fakeSyno) ListProjects(ctx context.Context) ([]synology.Project, error) {
	f.calls = append(f.calls, "list")
	return f.projects, f.fail["list"]
}
func (f *fakeSyno) ProjectAction(ctx context.Context, method, id string) error {
	f.calls = append(f.calls, method+" "+id)
	return f.fail[method]
}
func (f *fakeSyno) ContainerAction(ctx context.Context, method, name string) error {
	f.calls = append(f.calls, "container "+method+" "+name)
	return f.fail[method]
}
func (f *fakeSyno) ContainerDelete(ctx context.Context, name string, preserve, force bool) error {
	f.calls = append(f.calls, "container delete "+name)
	if !preserve || force {
		return errors.New("bad delete flags")
	}
	return f.fail["delete"]
}

func TestSynologyProject(t *testing.T) {
	f := &fakeSyno{projects: []synology.Project{{ID: "u1", Name: "myapp", Path: "/volume1/docker/myapp"}, {ID: "u2", Name: "other", Path: "/volume1/docker/other"}}}
	s := &Synology{Client: f, Logger: testLogger(), ProjectMap: map[string]string{"compose-name": "other"}}
	ctx := context.Background()
	// by name
	if err := s.Update(ctx, &Target{Kind: KindRebuild, Compose: &compose.Info{Project: "MyApp", Service: "web"}}); err != nil {
		t.Fatal(err)
	}
	// by map
	if err := s.Update(ctx, &Target{Kind: KindImage, Compose: &compose.Info{Project: "compose-name", Service: "web"}}); err != nil {
		t.Fatal(err)
	}
	// by path
	if err := s.Update(ctx, &Target{Kind: KindImage, Compose: &compose.Info{Project: "zzz", Service: "web", WorkingDir: "/volume1/docker/other/"}}); err != nil {
		t.Fatal(err)
	}
	// unknown
	err := s.Update(ctx, &Target{Compose: &compose.Info{Project: "nope", WorkingDir: "/x"}})
	if err == nil || !strings.Contains(err.Error(), "project_map") {
		t.Fatalf("got %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls, ",")
	want := "login,list,build_stream u1,start_stream u1,build_stream u2,start_stream u2,build_stream u2,start_stream u2,logout"
	if got != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
	}
	if !s.HandlesBuild() || s.HandlesPull() {
		t.Fatal("flags")
	}
}

func TestSynologyContainer(t *testing.T) {
	f := &fakeSyno{}
	s := &Synology{Client: f, Logger: testLogger()}
	if err := s.Update(context.Background(), &Target{ContainerName: "web", Image: "nginx:1"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls, ",")
	if got != "login,container stop web,container delete web,container start web" {
		t.Fatalf("calls %s", got)
	}
	f.fail = map[string]error{"start": errors.New("boom")}
	if err := s.Update(context.Background(), &Target{ContainerName: "web"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestSynologyBuildFailure(t *testing.T) {
	f := &fakeSyno{projects: []synology.Project{{ID: "u1", Name: "p"}}, fail: map[string]error{"build_stream": errors.New("build failed")}}
	s := &Synology{Client: f, Logger: testLogger()}
	err := s.Update(context.Background(), &Target{Compose: &compose.Info{Project: "p"}})
	if err == nil || !strings.Contains(err.Error(), "build failed") {
		t.Fatalf("got %v", err)
	}
}
