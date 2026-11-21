package build

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	buildtypes "github.com/docker/docker/api/types/build"

	"github.com/dansimau/dockwarden/internal/compose"
	"github.com/dansimau/dockwarden/internal/dockerapi/fake"
	"github.com/dansimau/dockwarden/internal/execx"
)

func TestComposeCLIBuild(t *testing.T) {
	runner := &execx.Fake{}
	b := &ComposeCLI{Runner: runner}
	info := compose.Info{Project: "p", Service: "web", WorkingDir: "/srv/p", ConfigFiles: []string{"/srv/p/compose.yml"}}
	if err := b.Build(context.Background(), Request{Info: info, Pull: true, NoCache: true}); err != nil {
		t.Fatal(err)
	}
	want := "docker compose -p p -f /srv/p/compose.yml --project-directory /srv/p build --pull --no-cache web"
	if got := runner.Strings()[0]; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCommandBuild(t *testing.T) {
	runner := &execx.Fake{}
	b, err := NewCommand(`ssh host "cd {{.WorkingDir}} && docker compose build {{if .Pull}}--pull {{end}}{{.Service}}" # {{.Tag}} {{.BaseImages}}`, "", runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	info := compose.Info{Project: "p", Service: "web", WorkingDir: "/srv/p"}
	if err := b.Build(context.Background(), Request{Info: info, Pull: true, Tag: "p-web", BaseImages: []string{"nginx:1"}}); err != nil {
		t.Fatal(err)
	}
	want := `ssh host "cd /srv/p && docker compose build --pull web" # p-web nginx:1`
	if got := runner.Commands[0].Args[1]; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestAPIBuild(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "node_modules", "x"), 0o755)
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM nginx\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("node_modules\n*.log\n!keep.log\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "node_modules", "x", "a.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "debug.log"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "keep.log"), []byte("x"), 0o644)

	fc := fake.New()
	var gotOpts buildtypes.ImageBuildOptions
	fc.BuildHook = func(opts buildtypes.ImageBuildOptions) error { gotOpts = opts; return nil }
	b := &API{Client: fc}
	spec := &compose.BuildSpec{Context: dir, Dockerfile: "Dockerfile", Args: map[string]string{"A": "1"}, Target: "final"}
	req := Request{Info: compose.Info{Project: "p", Service: "web"}, Spec: spec, Tag: "p-web", Pull: true, Labels: map[string]string{"l": "v"}}
	if err := b.Build(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if gotOpts.Tags[0] != "p-web" || *gotOpts.BuildArgs["A"] != "1" || gotOpts.Target != "final" || !gotOpts.PullParent || gotOpts.Labels["l"] != "v" {
		t.Fatalf("opts %+v", gotOpts)
	}

	// failure propagates from the stream
	fc.BuildHook = func(opts buildtypes.ImageBuildOptions) error { return errors.New("step failed") }
	if err := b.Build(context.Background(), req); err == nil || !strings.Contains(err.Error(), "step failed") {
		t.Fatalf("expected stream error, got %v", err)
	}
}

func TestWriteContextDockerignore(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "node_modules", "x"), 0o755)
	os.MkdirAll(filepath.Join(dir, "src", "deep"), 0o755)
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM nginx\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("node_modules\n*.log\n!keep.log\n**/tmp.txt\nDockerfile\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "node_modules", "x", "a.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "deep", "tmp.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "debug.log"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "keep.log"), []byte("x"), 0o644)

	var buf bytes.Buffer
	if err := writeContext(&buf, dir, "Dockerfile", nil); err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	sort.Strings(names)
	want := []string{".dockerignore", "Dockerfile", "keep.log", "src/", "src/deep/", "src/main.go"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", names, want)
	}
}

func TestWriteContextInlineDockerfile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644)
	var buf bytes.Buffer
	if err := writeContext(&buf, dir, ".dockwarden.Dockerfile", []byte("FROM x\n")); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	h, err := tr.Next()
	if err != nil || h.Name != ".dockwarden.Dockerfile" || h.Size != 7 {
		t.Fatalf("got %+v %v", h, err)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		p, s string
		want bool
	}{
		{"**/tmp.txt", "a/b/tmp.txt", true},
		{"**/tmp.txt", "tmp.txt", true},
		{"src/**", "src/a/b", true},
		{"src/**", "other/a", false},
		{"*.log", "a.log", true},
		{"*.log", "dir/a.log", false},
		{"dir", "dir", true},
	}
	for _, c := range cases {
		if got := globMatch(c.p, c.s); got != c.want {
			t.Errorf("globMatch(%q,%q)=%v", c.p, c.s, got)
		}
	}
}
