package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAndEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte(`
interval: 24h
strategy:
  type: synology
  synology:
    url: https://nas:5001
    project_map: {web: my-web}
notify:
  webhook:
    url: https://hooks.example/x
`), 0o600)
	pw := filepath.Join(dir, "pw")
	os.WriteFile(pw, []byte("secret\n"), 0o600)
	t.Setenv("DOCKWARDEN_DRY_RUN", "true")
	t.Setenv("DOCKWARDEN_SYNOLOGY_USERNAME", "admin")
	t.Setenv("DOCKWARDEN_SYNOLOGY_PASSWORD_FILE", pw)
	t.Setenv("DOCKWARDEN_CONTAINERS", "a, b")
	t.Setenv("DOCKWARDEN_BUILD_PULL", "false")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Interval != 24*time.Hour || !cfg.DryRun || cfg.Strategy.Type != "synology" {
		t.Fatalf("unexpected %+v", cfg)
	}
	if cfg.Strategy.Synology.Password != "secret" || cfg.Strategy.Synology.Username != "admin" {
		t.Fatalf("secrets not resolved: %+v", cfg.Strategy.Synology)
	}
	if len(cfg.Containers) != 2 || cfg.Containers[1] != "b" {
		t.Fatalf("containers %v", cfg.Containers)
	}
	if cfg.BuildPull() {
		t.Fatal("build pull should be false")
	}
	if cfg.Strategy.Synology.ProjectMap["web"] != "my-web" {
		t.Fatal("project map")
	}
	// defaults preserved
	if cfg.Registry.Timeout != 30*time.Second || cfg.Notify.Webhook.Method != "POST" {
		t.Fatal("defaults lost")
	}
}

func TestUnknownField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("bogus: 1\n"), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	cfg := Default()
	cfg.Strategy.Type = "command"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("expected template error, got %v", err)
	}
	cfg = Default()
	cfg.Strategy.Type = "synology"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "url") {
		t.Fatalf("expected url error, got %v", err)
	}
	cfg = Default()
	cfg.Build.Mode = "weird"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected mode error")
	}
}

func TestBadEnv(t *testing.T) {
	cfg := Default()
	err := cfg.ApplyEnv(func(k string) (string, bool) {
		if k == "DOCKWARDEN_INTERVAL" {
			return "soon", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("expected duration parse error")
	}
}
