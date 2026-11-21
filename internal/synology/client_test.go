package synology

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, f *fakeDSM) *Client {
	c, err := New(Options{URL: f.srv.URL, Username: "admin", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoginLogout(t *testing.T) {
	f := newFakeDSM(t)
	c := newTestClient(t, f)
	ctx := context.Background()
	if err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}
	if !c.LoggedIn() || c.sid != "SID123" || c.synotoken != "TOK" {
		t.Fatalf("session %q %q", c.sid, c.synotoken)
	}
	if err := c.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if c.LoggedIn() {
		t.Fatal("should be logged out")
	}
	calls := strings.Join(f.Calls(), ",")
	if calls != "SYNO.API.Info/query,SYNO.API.Auth/login,SYNO.API.Auth/logout" {
		t.Fatalf("calls %s", calls)
	}
}

func TestLoginBadPassword(t *testing.T) {
	f := newFakeDSM(t)
	c, _ := New(Options{URL: f.srv.URL, Username: "admin", Password: "nope"})
	err := c.Login(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 400 || !strings.Contains(err.Error(), "incorrect password") {
		t.Fatalf("got %v", err)
	}
}

func TestProjects(t *testing.T) {
	f := newFakeDSM(t)
	f.projects["uuid-1"] = Project{ID: "uuid-1", Name: "myapp", Path: "/volume1/docker/myapp", Status: "RUNNING"}
	c := newTestClient(t, f)
	ctx := context.Background()
	if err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}
	ps, err := c.ListProjects(ctx)
	if err != nil || len(ps) != 1 || ps[0].Name != "myapp" {
		t.Fatalf("list: %v %v", ps, err)
	}
	p, err := c.GetProject(ctx, "uuid-1")
	if err != nil || p.Path != "/volume1/docker/myapp" {
		t.Fatalf("get: %v %v", p, err)
	}
	if err := c.ProjectAction(ctx, ProjectBuild, "uuid-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.ProjectAction(ctx, ProjectStart, "nope"); err == nil {
		t.Fatal("expected error for unknown project")
	}
}

func TestContainers(t *testing.T) {
	f := newFakeDSM(t)
	c := newTestClient(t, f)
	ctx := context.Background()
	if err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerAction(ctx, ContainerStop, "web"); err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerDelete(ctx, "web", true, false); err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerAction(ctx, ContainerStart, "web"); err != nil {
		t.Fatal(err)
	}
	cs, err := c.ListContainers(ctx)
	if err != nil || len(cs) != 1 || cs[0].Name != "web" {
		t.Fatalf("list %v %v", cs, err)
	}
}

func TestPermissionError(t *testing.T) {
	f := newFakeDSM(t)
	f.fail["SYNO.Docker.Project/list"] = 105
	c := newTestClient(t, f)
	ctx := context.Background()
	c.Login(ctx)
	_, err := c.ListProjects(ctx)
	if err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("got %v", err)
	}
}

func TestNotLoggedIn(t *testing.T) {
	f := newFakeDSM(t)
	c := newTestClient(t, f)
	_, err := c.ListProjects(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 119 {
		t.Fatalf("got %v", err)
	}
}

func TestJSONParams(t *testing.T) {
	v := jsonParams(map[string]any{"name": "web", "force": false, "limit": -1})
	if v.Get("name") != `"web"` || v.Get("force") != "false" || v.Get("limit") != "-1" {
		t.Fatalf("got %v", v)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := New(Options{URL: "::bad"}); err == nil {
		t.Fatal("expected error")
	}
}
