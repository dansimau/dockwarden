package compose

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeProjectName(t *testing.T) {
	cases := map[string]string{
		"MyApp":          "myapp",
		"my app":         "myapp",
		"my.app":         "myapp",
		"_my-app_":       "my-app_",
		"--Hello World!": "helloworld",
		"nextcloud-2":    "nextcloud-2",
		"Ünïcode":        "ncode",
		"":               "",
	}
	for in, want := range cases {
		if got := NormalizeProjectName(in); got != want {
			t.Errorf("NormalizeProjectName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeriveProjectName(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "x")
	os.Unsetenv("COMPOSE_PROJECT_NAME")
	dir := t.TempDir()
	// 3. basename of the directory
	if got := DeriveProjectName(nil, "/volume1/docker/My App", ""); got != "myapp" {
		t.Fatalf("basename: %q", got)
	}
	// 1. top-level name wins
	if got := DeriveProjectName([]byte("name: Explicit-Name\nservices: {}\n"), "/volume1/docker/x", ""); got != "explicit-name" {
		t.Fatalf("name key: %q", got)
	}
	// 2. COMPOSE_PROJECT_NAME from .env
	envFile := filepath.Join(dir, ".env")
	os.WriteFile(envFile, []byte("# comment\nCOMPOSE_PROJECT_NAME=From Env\n"), 0o600)
	if got := DeriveProjectName([]byte("services: {}\n"), dir, envFile); got != "fromenv" {
		t.Fatalf(".env: %q", got)
	}
	// name key interpolated from .env
	os.WriteFile(envFile, []byte("STACK=stack1\n"), 0o600)
	if got := DeriveProjectName([]byte("name: ${STACK}-web\n"), dir, envFile); got != "stack1-web" {
		t.Fatalf("interpolated name: %q", got)
	}
	// missing env file is fine
	if got := DeriveProjectName([]byte("x: 1\n"), "/srv/stack_two", filepath.Join(dir, "nope")); got != "stack_two" {
		t.Fatalf("missing env: %q", got)
	}
	// unparsable YAML falls back to the directory
	if got := DeriveProjectName([]byte("::: not yaml"), "/srv/Fallback", ""); got != "fallback" {
		t.Fatalf("bad yaml: %q", got)
	}
}
