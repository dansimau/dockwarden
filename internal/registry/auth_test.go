package registry

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestDockerConfigAuths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	auth := base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	os.WriteFile(path, []byte(`{"auths":{"https://index.docker.io/v1/":{"auth":"`+auth+`"},"ghcr.io":{"username":"bob","password":"pw"}},"credHelpers":{"gcr.io":"gcr"}}`), 0o600)
	cfg, err := LoadDockerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunHelper = func(helper, server string) ([]byte, error) {
		if helper != "gcr" || server != "gcr.io" {
			t.Fatalf("unexpected helper call %s %s", helper, server)
		}
		return []byte(`{"Username":"_json_key","Secret":"json"}`), nil
	}
	c, _ := cfg.Get("registry-1.docker.io")
	if c.Username != "alice" || c.Password != "s3cret" {
		t.Fatalf("docker.io creds: %+v", c)
	}
	c, _ = cfg.Get("ghcr.io")
	if c.Username != "bob" || c.Password != "pw" {
		t.Fatalf("ghcr creds: %+v", c)
	}
	c, _ = cfg.Get("gcr.io")
	if c.Username != "_json_key" || c.Password != "json" {
		t.Fatalf("gcr creds: %+v", c)
	}
	c, _ = cfg.Get("unknown.example")
	if !c.IsZero() {
		t.Fatalf("expected no creds, got %+v", c)
	}
}

func TestLoadMissingConfig(t *testing.T) {
	cfg, err := LoadDockerConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || cfg == nil {
		t.Fatalf("missing config should be ok: %v", err)
	}
}

func TestCredsStoreToken(t *testing.T) {
	cfg := &DockerConfig{CredsStore: "desktop", RunHelper: func(helper, server string) ([]byte, error) {
		return []byte(`{"Username":"<token>","Secret":"tok"}`), nil
	}}
	c, err := cfg.Get("example.com")
	if err != nil || c.IdentityToken != "tok" {
		t.Fatalf("got %+v %v", c, err)
	}
}

func TestEncodeDockerAuth(t *testing.T) {
	if EncodeDockerAuth(Credentials{}, "x") != "" {
		t.Fatal("empty creds should encode to empty string")
	}
	s := EncodeDockerAuth(Credentials{Username: "u", Password: "p"}, "ghcr.io")
	raw, err := base64.URLEncoding.DecodeString(s)
	if err != nil || string(raw) == "" {
		t.Fatal("bad encoding")
	}
}
