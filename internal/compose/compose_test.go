package compose

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFromLabels(t *testing.T) {
	info, ok := FromLabels(map[string]string{
		LabelProject: "myproj", LabelService: "web", LabelWorkingDir: "/srv/myproj",
		LabelConfigFiles: "/srv/myproj/docker-compose.yml,/srv/myproj/override.yml",
	})
	if !ok || info.Project != "myproj" || info.Service != "web" || len(info.ConfigFiles) != 2 {
		t.Fatalf("got %+v %v", info, ok)
	}
	if _, ok := FromLabels(map[string]string{"foo": "bar"}); ok {
		t.Fatal("non-compose labels should not match")
	}
}

func TestPathMapper(t *testing.T) {
	m := PathMapper{"/volume1/docker": "/projects", "/volume1": "/vol"}
	if got := m.Map("/volume1/docker/app/compose.yml"); got != "/projects/app/compose.yml" {
		t.Fatalf("got %s", got)
	}
	if got := m.Map("/volume1/other/x"); got != "/vol/other/x" {
		t.Fatalf("got %s", got)
	}
	if got := m.Map("/elsewhere"); got != "/elsewhere" {
		t.Fatalf("got %s", got)
	}
	if got := (PathMapper{}).Map("/a"); got != "/a" {
		t.Fatalf("got %s", got)
	}
}

func writeProject(t *testing.T) string {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "app"), 0o755)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("TAG=1.2\nNAME=\"quoted\"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(`
services:
  web:
    image: myimg:${TAG}
    build:
      context: ./app
      dockerfile: Dockerfile.prod
      args:
        - BASE_TAG=${TAG}
        - FROM_ENV
      target: runtime
  db:
    image: postgres:16
  simple:
    build: .
`), 0o600)
	os.WriteFile(filepath.Join(dir, "override.yml"), []byte(`
services:
  web:
    build:
      args:
        EXTRA: "${NAME:-x}"
`), 0o600)
	os.WriteFile(filepath.Join(dir, "app", "Dockerfile.prod"), []byte("ARG BASE_TAG=latest\nFROM nginx:${BASE_TAG}\n"), 0o600)
	return dir
}

func TestLoadBuildSpec(t *testing.T) {
	dir := writeProject(t)
	t.Setenv("FROM_ENV", "envval")
	info := Info{Project: "p", Service: "web", WorkingDir: dir, ConfigFiles: []string{filepath.Join(dir, "docker-compose.yml"), filepath.Join(dir, "override.yml")}}
	spec, err := LoadBuildSpec(info, nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Context != filepath.Join(dir, "app") || spec.Dockerfile != "Dockerfile.prod" || spec.Target != "runtime" {
		t.Fatalf("got %+v", spec)
	}
	if spec.Image != "myimg:1.2" {
		t.Fatalf("image %q", spec.Image)
	}
	// override file merges args (shallow override replaces args map)
	if spec.Args["EXTRA"] != "quoted" {
		t.Fatalf("args %v", spec.Args)
	}
	// first-file-only args
	spec1, err := LoadBuildSpec(Info{Project: "p", Service: "web", WorkingDir: dir, ConfigFiles: []string{filepath.Join(dir, "docker-compose.yml")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec1.Args["BASE_TAG"] != "1.2" || spec1.Args["FROM_ENV"] != "envval" {
		t.Fatalf("args %v", spec1.Args)
	}
	df, err := spec1.ReadDockerfile()
	if err != nil || len(df) == 0 {
		t.Fatal("dockerfile not readable")
	}
}

func TestLoadBuildSpecNoBuild(t *testing.T) {
	dir := writeProject(t)
	info := Info{Project: "p", Service: "db", WorkingDir: dir, ConfigFiles: []string{filepath.Join(dir, "docker-compose.yml")}}
	if _, err := LoadBuildSpec(info, nil); err != ErrNoBuild {
		t.Fatalf("expected ErrNoBuild, got %v", err)
	}
}

func TestLoadBuildSpecStringForm(t *testing.T) {
	dir := writeProject(t)
	info := Info{Project: "p", Service: "simple", WorkingDir: dir, ConfigFiles: []string{filepath.Join(dir, "docker-compose.yml")}}
	spec, err := LoadBuildSpec(info, nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Context != dir || spec.Dockerfile != "Dockerfile" {
		t.Fatalf("got %+v", spec)
	}
}

func TestLoadBuildSpecWithPathMap(t *testing.T) {
	dir := writeProject(t)
	info := Info{Project: "p", Service: "simple", WorkingDir: "/volume1/docker/p", ConfigFiles: []string{"/volume1/docker/p/docker-compose.yml"}}
	spec, err := LoadBuildSpec(info, PathMapper{"/volume1/docker/p": dir})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Context != dir {
		t.Fatalf("context %s", spec.Context)
	}
	if _, err := LoadBuildSpec(info, nil); err == nil {
		t.Fatal("expected error for unmapped path")
	}
}

func TestRelativeConfigFiles(t *testing.T) {
	info := Info{WorkingDir: "/srv/p", ConfigFiles: []string{"docker-compose.yml"}}
	if got := info.LocalConfigFiles(nil); got[0] != "/srv/p/docker-compose.yml" {
		t.Fatalf("got %v", got)
	}
}
