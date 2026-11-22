package strategy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dansimau/dockwarden/internal/compose"
	"github.com/dansimau/dockwarden/internal/synology"
	"github.com/dansimau/dockwarden/internal/synology/synotest"
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
	// list does not include content, like DSM
	out := make([]synology.Project, len(f.projects))
	for i, p := range f.projects {
		p.Content = ""
		out[i] = p
	}
	return out, f.fail["list"]
}
func (f *fakeSyno) GetProject(ctx context.Context, id string) (*synology.Project, error) {
	f.calls = append(f.calls, "get "+id)
	if err := f.fail["get"]; err != nil {
		return nil, err
	}
	for _, p := range f.projects {
		if p.ID == id {
			return &p, nil
		}
	}
	return nil, errors.New("no such project")
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

func TestSynologyAutoMapping(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "envproj"), 0o755)
	os.WriteFile(filepath.Join(dir, "envproj", ".env"), []byte("COMPOSE_PROJECT_NAME=from-env\n"), 0o600)
	f := &fakeSyno{projects: []synology.Project{
		{ID: "u1", Name: "Media Stack", Path: "/volume1/docker/Media Stack", Content: "services: {}\n"},        // -> mediastack (dir)
		{ID: "u2", Name: "named", Path: "/volume1/docker/named", Content: "name: Custom_Name\nservices: {}\n"}, // -> custom_name (name:)
		{ID: "u3", Name: "envproj", Path: "/volume1/docker/envproj", Content: "services: {}\n"},                // -> from-env (.env via path map)
		{ID: "u4", Name: "DSM Only", Path: "/volume1/docker/dsmonly", Content: "services: {}\n"},               // -> dsmonly
	}}
	s := &Synology{Client: f, Logger: testLogger(), PathMap: compose.PathMapper{"/volume1/docker": dir}, ProjectMap: map[string]string{"override": "DSM Only", "byid": "u2"}}
	ctx := context.Background()
	s.BeginRun(ctx)
	find := func(composeName, wd string) string {
		p, err := s.FindProject(ctx, &compose.Info{Project: composeName, WorkingDir: wd})
		if err != nil {
			return "ERR " + err.Error()
		}
		return p.ID
	}
	cases := map[[2]string]string{
		{"mediastack", ""}:                 "u1",
		{"custom_name", ""}:                "u2",
		{"from-env", ""}:                   "u3",
		{"dsmonly", ""}:                    "u4",
		{"override", ""}:                   "u4", // manual map by DSM name
		{"byid", ""}:                       "u2", // manual map by DSM id
		{"named", ""}:                      "u2", // falls back to DSM name match
		{"zzz", "/volume1/docker/envproj"}: "u3", // falls back to path match
	}
	for in, want := range cases {
		if got := find(in[0], in[1]); got != want {
			t.Errorf("FindProject(%q, %q) = %s, want %s", in[0], in[1], got, want)
		}
	}
	if got := find("unknown", "/nowhere"); !strings.Contains(got, "project_map") {
		t.Errorf("unknown project: %s", got)
	}
	s.ProjectMap["bad"] = "nope"
	if got := find("bad", ""); !strings.Contains(got, "no DSM project has that name") {
		t.Errorf("bad override: %s", got)
	}
	// projects fetched once per run, each project's content fetched once
	if n := len(f.calls); n != 5 { // list + 4 gets
		t.Fatalf("expected 5 calls, got %v", f.calls)
	}
	ms, err := s.Mappings(ctx)
	if err != nil || len(ms) != 4 || ms[0].ComposeName != "custom_name" || ms[3].ComposeName != "mediastack" {
		t.Fatalf("mappings %+v %v", ms, err)
	}
}

func TestSynologyDuplicateDerivedNames(t *testing.T) {
	f := &fakeSyno{projects: []synology.Project{
		{ID: "u1", Name: "a", Path: "/volume1/docker/same", Content: "services: {}\n"},
		{ID: "u2", Name: "b", Path: "/volume2/docker/same", Content: "services: {}\n"},
	}}
	s := &Synology{Client: f, Logger: testLogger()}
	p, err := s.FindProject(context.Background(), &compose.Info{Project: "same"})
	if err != nil || p.ID != "u1" {
		t.Fatalf("first project should win: %v %v", p, err)
	}
	p, err = s.FindProject(context.Background(), &compose.Info{Project: "x", WorkingDir: "/volume2/docker/same"})
	if err != nil || p.ID != "u2" {
		t.Fatalf("path match: %v %v", p, err)
	}
}

func TestSynologyGetFailureFallsBackToPath(t *testing.T) {
	f := &fakeSyno{projects: []synology.Project{{ID: "u1", Name: "p", Path: "/volume1/docker/stack"}}, fail: map[string]error{"get": errors.New("boom")}}
	s := &Synology{Client: f, Logger: testLogger()}
	p, err := s.FindProject(context.Background(), &compose.Info{Project: "stack"})
	if err != nil || p.ID != "u1" {
		t.Fatalf("got %v %v", p, err)
	}
}

func TestSynologyProjectUpdate(t *testing.T) {
	f := &fakeSyno{projects: []synology.Project{{ID: "u1", Name: "myapp", Path: "/volume1/docker/myapp", Content: "services: {}\n"}, {ID: "u2", Name: "other", Path: "/volume1/docker/other", Content: "services: {}\n"}}}
	s := &Synology{Client: f, Logger: testLogger(), ProjectMap: map[string]string{"compose-name": "other"}}
	ctx := context.Background()
	s.BeginRun(ctx)
	if err := s.Update(ctx, &Target{Kind: KindRebuild, Compose: &compose.Info{Project: "myapp", Service: "web"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, &Target{Kind: KindImage, Compose: &compose.Info{Project: "compose-name", Service: "web"}}); err != nil {
		t.Fatal(err)
	}
	// same project again in the same run -> deduped
	if err := s.Update(ctx, &Target{Kind: KindImage, Compose: &compose.Info{Project: "other", Service: "db"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.EndRun(ctx); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls, ",")
	want := "login,list,get u1,get u2,build_stream u1,start_stream u1,build_stream u2,start_stream u2,logout"
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
	f := &fakeSyno{projects: []synology.Project{{ID: "u1", Name: "p", Path: "/x/p"}}, fail: map[string]error{"build_stream": errors.New("build failed")}}
	s := &Synology{Client: f, Logger: testLogger()}
	err := s.Update(context.Background(), &Target{Compose: &compose.Info{Project: "p"}})
	if err == nil || !strings.Contains(err.Error(), "build failed") {
		t.Fatalf("got %v", err)
	}
}

// TestSynologyEndToEndWithFakeDSM drives the real synology.Client against
// the httptest DSM mock.
func TestSynologyEndToEndWithFakeDSM(t *testing.T) {
	dsm := synotest.New(t)
	dsm.Projects["aaaa-1"] = synotest.Project{ID: "aaaa-1", Name: "Home Automation", Path: "/volume1/docker/home-automation", Status: "RUNNING", Content: "services:\n  ha:\n    build: .\n"}
	dsm.Projects["bbbb-2"] = synotest.Project{ID: "bbbb-2", Name: "Blog", Path: "/volume1/docker/blog", Status: "RUNNING", Content: "name: my-blog\nservices: {}\n"}
	client, err := synology.New(synology.Options{URL: dsm.URL, Username: "admin", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Synology{Client: client, Logger: testLogger()}
	ctx := context.Background()
	s.BeginRun(ctx)
	// derived from directory name
	if err := s.Update(ctx, &Target{Kind: KindRebuild, Compose: &compose.Info{Project: "home-automation", Service: "ha", WorkingDir: "/volume1/docker/home-automation"}}); err != nil {
		t.Fatal(err)
	}
	// derived from name: key
	if err := s.Update(ctx, &Target{Kind: KindImage, Compose: &compose.Info{Project: "my-blog", Service: "web"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, &Target{Kind: KindImage, Compose: &compose.Info{Project: "nope", Service: "web"}}); err == nil {
		t.Fatal("expected mapping error")
	}
	s.EndRun(ctx)
	got := strings.Join(dsm.Calls(), ",")
	want := "SYNO.API.Info/query,SYNO.API.Auth/login,SYNO.Docker.Project/list,SYNO.Docker.Project/get,SYNO.Docker.Project/get," +
		"SYNO.Docker.Project/build_stream,SYNO.Docker.Project/start_stream,SYNO.Docker.Project/build_stream,SYNO.Docker.Project/start_stream,SYNO.API.Auth/logout"
	if got != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
	}
}
