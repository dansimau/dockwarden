package registry

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Credentials holds registry credentials.
type Credentials struct {
	Username string
	Password string
	// IdentityToken is set when a credential helper returns a token instead of a password.
	IdentityToken string
}

// IsZero reports whether no credentials are present.
func (c Credentials) IsZero() bool {
	return c.Username == "" && c.Password == "" && c.IdentityToken == ""
}

// CredentialStore resolves credentials for a registry host.
type CredentialStore interface {
	Get(registryHost string) (Credentials, error)
}

// NoCredentials is a CredentialStore that never returns credentials.
type NoCredentials struct{}

// Get implements CredentialStore.
func (NoCredentials) Get(string) (Credentials, error) { return Credentials{}, nil }

// StaticCredentials maps registry hosts to credentials.
type StaticCredentials map[string]Credentials

// Get implements CredentialStore.
func (s StaticCredentials) Get(host string) (Credentials, error) {
	for _, k := range hostKeys(host) {
		if c, ok := s[k]; ok {
			return c, nil
		}
	}
	return Credentials{}, nil
}

// DockerConfig is a CredentialStore backed by a Docker CLI config.json file,
// supporting inline `auths`, `credHelpers` and `credsStore`.
type DockerConfig struct {
	Auths       map[string]dockerAuthEntry `json:"auths"`
	CredHelpers map[string]string          `json:"credHelpers"`
	CredsStore  string                     `json:"credsStore"`

	// RunHelper executes a credential helper; replaced in tests.
	RunHelper func(helper, serverURL string) ([]byte, error) `json:"-"`
}

type dockerAuthEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

// DefaultDockerConfigPath returns the config.json path honouring DOCKER_CONFIG.
func DefaultDockerConfigPath() string {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return filepath.Join(d, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".docker", "config.json")
}

// LoadDockerConfig parses a Docker config.json. A missing file yields an
// empty config and no error.
func LoadDockerConfig(path string) (*DockerConfig, error) {
	cfg := &DockerConfig{RunHelper: runHelper}
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// Get implements CredentialStore.
func (d *DockerConfig) Get(host string) (Credentials, error) {
	keys := hostKeys(host)
	for _, k := range keys {
		if helper, ok := d.CredHelpers[k]; ok {
			return d.fromHelper(helper, k)
		}
	}
	for _, k := range keys {
		if e, ok := d.Auths[k]; ok {
			c, err := e.credentials()
			if err != nil {
				return Credentials{}, fmt.Errorf("auths[%q]: %w", k, err)
			}
			if !c.IsZero() {
				return c, nil
			}
		}
	}
	if d.CredsStore != "" {
		for _, k := range keys {
			c, err := d.fromHelper(d.CredsStore, k)
			if err == nil && !c.IsZero() {
				return c, nil
			}
		}
	}
	return Credentials{}, nil
}

func (e dockerAuthEntry) credentials() (Credentials, error) {
	if e.Auth != "" {
		raw, err := base64.StdEncoding.DecodeString(e.Auth)
		if err != nil {
			return Credentials{}, fmt.Errorf("decode auth: %w", err)
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return Credentials{}, errors.New("auth is not user:password")
		}
		return Credentials{Username: user, Password: pass, IdentityToken: e.IdentityToken}, nil
	}
	return Credentials{Username: e.Username, Password: e.Password, IdentityToken: e.IdentityToken}, nil
}

func (d *DockerConfig) fromHelper(helper, serverURL string) (Credentials, error) {
	run := d.RunHelper
	if run == nil {
		run = runHelper
	}
	out, err := run(helper, serverURL)
	if err != nil {
		return Credentials{}, err
	}
	var resp struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return Credentials{}, fmt.Errorf("credential helper %s: bad output: %w", helper, err)
	}
	if resp.Username == "<token>" {
		return Credentials{IdentityToken: resp.Secret}, nil
	}
	return Credentials{Username: resp.Username, Password: resp.Secret}, nil
}

func runHelper(helper, serverURL string) ([]byte, error) {
	cmd := exec.Command("docker-credential-"+helper, "get")
	cmd.Stdin = strings.NewReader(serverURL)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "credentials not found") {
			return []byte("{}"), nil
		}
		return nil, fmt.Errorf("docker-credential-%s: %w: %s", helper, err, msg)
	}
	return stdout.Bytes(), nil
}

// hostKeys returns the config.json keys to try for a registry host, most
// specific first. Docker Hub is stored under its legacy index URL.
func hostKeys(host string) []string {
	host = strings.TrimSuffix(host, "/")
	keys := []string{host, "https://" + host, "http://" + host}
	switch host {
	case "docker.io", "registry-1.docker.io", "index.docker.io":
		keys = append([]string{"https://index.docker.io/v1/", "index.docker.io", "docker.io", "registry-1.docker.io"}, keys...)
	}
	// dedupe preserving order
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// EncodeDockerAuth encodes credentials as the base64 JSON blob expected by the
// Docker Engine API X-Registry-Auth header.
func EncodeDockerAuth(c Credentials, serverAddress string) string {
	if c.IsZero() {
		return ""
	}
	b, _ := json.Marshal(map[string]string{
		"username":      c.Username,
		"password":      c.Password,
		"identitytoken": c.IdentityToken,
		"serveraddress": serverAddress,
	})
	return base64.URLEncoding.EncodeToString(b)
}
