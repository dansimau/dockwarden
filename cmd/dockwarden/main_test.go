package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionAndHelp(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "dockwarden") {
		t.Fatalf("version: %d %s", code, out.String())
	}
	out.Reset()
	if code := run([]string{"--help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Usage") {
		t.Fatalf("help: %d %s", code, out.String())
	}
}

func TestBadConfig(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"--config", "/nonexistent.yaml"}, &out, &errb); code != ExitFatal {
		t.Fatalf("code %d", code)
	}
	if code := run([]string{"--strategy", "bogus"}, &out, &errb); code != ExitFatal || !strings.Contains(errb.String(), "strategy.type") {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if code := run([]string{"--bogus-flag"}, &out, &errb); code != ExitFatal {
		t.Fatalf("code %d", code)
	}
	if code := run([]string{"frobnicate"}, &out, &errb); code != ExitFatal {
		t.Fatalf("code %d", code)
	}
}

func TestDockerUnreachableIsFatal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(dir, "nope.sock"))
	t.Setenv("DOCKER_CONFIG", dir)
	cfg := filepath.Join(dir, "c.yaml")
	os.WriteFile(cfg, []byte("state_file: "+filepath.Join(dir, "state.json")+"\nbuild:\n  mode: api\nstrategy:\n  docker:\n    compose_cli: never\n"), 0o600)
	var out, errb bytes.Buffer
	if code := run([]string{"--config", cfg, "--once", "--log-format", "json"}, &out, &errb); code != ExitFatal {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), `"level":"ERROR"`) {
		t.Fatalf("expected structured error log, got %s", errb.String())
	}
}

func TestSynologyUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"synology"}, &out, &errb); code != ExitFatal {
		t.Fatalf("code %d", code)
	}
	t.Setenv("DOCKWARDEN_SYNOLOGY_URL", "")
	if code := run([]string{"synology", "projects"}, &out, &errb); code != ExitFatal || !strings.Contains(errb.String(), "url") {
		t.Fatalf("code %d: %s", code, errb.String())
	}
}
