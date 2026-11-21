// Package config loads dockwarden configuration from a YAML file with
// environment variable overrides.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration.
type Config struct {
	Docker   DockerConfig   `yaml:"docker"`
	Registry RegistryConfig `yaml:"registry"`
	Log      LogConfig      `yaml:"log"`

	// Interval between runs; 0 means run once and exit.
	Interval time.Duration `yaml:"interval"`
	// RunOnStart runs immediately when Interval is set (default true).
	RunOnStart *bool `yaml:"run_on_start"`

	DryRun      bool `yaml:"dry_run"`
	MonitorOnly bool `yaml:"monitor_only"`
	// LabelEnable switches to opt-in mode: only containers labelled
	// com.dockwarden.enable=true are considered.
	LabelEnable bool `yaml:"label_enable"`
	// IncludeStopped also considers stopped containers.
	IncludeStopped bool `yaml:"include_stopped"`
	// Containers restricts the run to the named containers (empty = all).
	Containers []string `yaml:"containers"`
	// Cleanup removes the old image after a successful update.
	Cleanup bool `yaml:"cleanup"`
	// StateFile stores base image digests for rebuilt compose services.
	StateFile string `yaml:"state_file"`
	// StopTimeout is how long to wait for a container to stop gracefully.
	StopTimeout time.Duration `yaml:"stop_timeout"`
	// ExitCodeOnUpdates, if non-zero, is returned when updates are available
	// but were not applied (dry run / monitor only).
	ExitCodeOnUpdates int `yaml:"exit_code_on_updates"`

	Compose  ComposeConfig  `yaml:"compose"`
	Build    BuildConfig    `yaml:"build"`
	Strategy StrategyConfig `yaml:"strategy"`
	Notify   NotifyConfig   `yaml:"notify"`
}

// DockerConfig configures the Docker Engine connection.
type DockerConfig struct {
	// Host overrides DOCKER_HOST (e.g. unix:///var/run/docker.sock).
	Host string `yaml:"host"`
	// ConfigFile is the Docker CLI config.json used for registry credentials.
	ConfigFile string `yaml:"config_file"`
}

// RegistryConfig configures registry access.
type RegistryConfig struct {
	Timeout       time.Duration `yaml:"timeout"`
	SkipTLSVerify bool          `yaml:"skip_tls_verify"`
	// Insecure lists registries to contact over plain HTTP.
	Insecure []string `yaml:"insecure"`
}

// LogConfig configures logging.
type LogConfig struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // text or json
}

// ComposeConfig configures compose project detection.
type ComposeConfig struct {
	// PathMap rewrites host paths from compose labels to paths visible inside
	// the dockwarden container (host prefix -> local prefix). Not needed when
	// project directories are mounted at identical paths.
	PathMap map[string]string `yaml:"path_map"`
}

// BuildConfig configures how compose services with `build:` are rebuilt.
type BuildConfig struct {
	// Mode: "compose" (docker compose build via CLI), "api" (Docker Engine
	// ImageBuild), "command" (custom command) or "auto" (compose if the CLI
	// is available, otherwise api).
	Mode string `yaml:"mode"`
	// Command template for mode=command.
	Command string `yaml:"command"`
	// Pull passes --pull to the builder so base images are refreshed.
	Pull *bool `yaml:"pull"`
	// NoCache disables the build cache.
	NoCache bool `yaml:"no_cache"`
	// Timeout for a single build.
	Timeout time.Duration `yaml:"timeout"`
}

// StrategyConfig selects and configures the update/restart strategy.
type StrategyConfig struct {
	Type     string          `yaml:"type"` // docker, command, synology
	Docker   DockerStrategy  `yaml:"docker"`
	Command  CommandStrategy `yaml:"command"`
	Synology SynologyConfig  `yaml:"synology"`
}

// DockerStrategy configures the native docker strategy.
type DockerStrategy struct {
	// ComposeCLI: "auto", "always" or "never" — use `docker compose up -d`
	// for compose containers instead of recreating via the API.
	ComposeCLI string `yaml:"compose_cli"`
}

// CommandStrategy configures the custom command strategy.
type CommandStrategy struct {
	// Template is a Go text/template rendered with the update target and run
	// via the shell.
	Template string `yaml:"template"`
	Shell    string `yaml:"shell"`
	// HandlesBuild indicates the command performs the rebuild itself, so
	// dockwarden only pulls newer base images and skips its own build step.
	HandlesBuild bool `yaml:"handles_build"`
	// HandlesPull indicates the command performs the image pull itself.
	HandlesPull bool          `yaml:"handles_pull"`
	Timeout     time.Duration `yaml:"timeout"`
}

// SynologyConfig configures the Synology Container Manager strategy.
type SynologyConfig struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// UsernameFile/PasswordFile read the secret from a file (e.g. a Docker secret).
	UsernameFile       string `yaml:"username_file"`
	PasswordFile       string `yaml:"password_file"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	// ProjectMap maps compose project names to Synology project names when
	// they differ. By default the compose project name (and then its path)
	// is used.
	ProjectMap map[string]string `yaml:"project_map"`
	// Timeout for API calls (project builds can take a while).
	Timeout time.Duration `yaml:"timeout"`
}

// NotifyConfig configures notifications.
type NotifyConfig struct {
	Webhook WebhookConfig `yaml:"webhook"`
}

// WebhookConfig configures a generic webhook notification.
type WebhookConfig struct {
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Headers map[string]string `yaml:"headers"`
	// Template renders the body; default is a JSON summary.
	Template string `yaml:"template"`
	// On: "changes" (updates found or errors; default), "errors", "always".
	On      string        `yaml:"on"`
	Timeout time.Duration `yaml:"timeout"`
}

// Default returns the default configuration.
func Default() *Config {
	t := true
	return &Config{
		Registry:    RegistryConfig{Timeout: 30 * time.Second},
		Log:         LogConfig{Level: "info", Format: "text"},
		StateFile:   "/var/lib/dockwarden/state.json",
		StopTimeout: 30 * time.Second,
		RunOnStart:  &t,
		Build:       BuildConfig{Mode: "auto", Pull: &t, Timeout: 30 * time.Minute},
		Strategy: StrategyConfig{
			Type:     "docker",
			Docker:   DockerStrategy{ComposeCLI: "auto"},
			Command:  CommandStrategy{Shell: "/bin/sh", Timeout: 10 * time.Minute},
			Synology: SynologyConfig{Timeout: 10 * time.Minute},
		},
		Notify: NotifyConfig{Webhook: WebhookConfig{Method: "POST", On: "changes", Timeout: 15 * time.Second}},
	}
}

// Load reads the YAML file at path (if non-empty) on top of defaults, then
// applies environment overrides.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := cfg.ApplyEnv(os.LookupEnv); err != nil {
		return nil, err
	}
	return cfg, nil
}

// EnvPrefix is the prefix for environment overrides.
const EnvPrefix = "DOCKWARDEN_"

// ApplyEnv applies DOCKWARDEN_* environment overrides.
func (c *Config) ApplyEnv(lookup func(string) (string, bool)) error {
	var errs []error
	str := func(key string, dst *string) {
		if v, ok := lookup(EnvPrefix + key); ok {
			*dst = v
		}
	}
	boolean := func(key string, dst *bool) {
		if v, ok := lookup(EnvPrefix + key); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s%s: %w", EnvPrefix, key, err))
				return
			}
			*dst = b
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v, ok := lookup(EnvPrefix + key); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s%s: %w", EnvPrefix, key, err))
				return
			}
			*dst = d
		}
	}
	integer := func(key string, dst *int) {
		if v, ok := lookup(EnvPrefix + key); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s%s: %w", EnvPrefix, key, err))
				return
			}
			*dst = n
		}
	}
	list := func(key string, dst *[]string) {
		if v, ok := lookup(EnvPrefix + key); ok {
			*dst = nil
			for _, s := range strings.Split(v, ",") {
				if s = strings.TrimSpace(s); s != "" {
					*dst = append(*dst, s)
				}
			}
		}
	}

	str("DOCKER_HOST", &c.Docker.Host)
	str("DOCKER_CONFIG_FILE", &c.Docker.ConfigFile)
	dur("REGISTRY_TIMEOUT", &c.Registry.Timeout)
	boolean("REGISTRY_SKIP_TLS_VERIFY", &c.Registry.SkipTLSVerify)
	list("REGISTRY_INSECURE", &c.Registry.Insecure)
	str("LOG_LEVEL", &c.Log.Level)
	str("LOG_FORMAT", &c.Log.Format)
	dur("INTERVAL", &c.Interval)
	boolean("DRY_RUN", &c.DryRun)
	boolean("MONITOR_ONLY", &c.MonitorOnly)
	boolean("LABEL_ENABLE", &c.LabelEnable)
	boolean("INCLUDE_STOPPED", &c.IncludeStopped)
	list("CONTAINERS", &c.Containers)
	boolean("CLEANUP", &c.Cleanup)
	str("STATE_FILE", &c.StateFile)
	dur("STOP_TIMEOUT", &c.StopTimeout)
	integer("EXIT_CODE_ON_UPDATES", &c.ExitCodeOnUpdates)
	str("BUILD_MODE", &c.Build.Mode)
	str("BUILD_COMMAND", &c.Build.Command)
	boolean("BUILD_NO_CACHE", &c.Build.NoCache)
	dur("BUILD_TIMEOUT", &c.Build.Timeout)
	str("STRATEGY", &c.Strategy.Type)
	str("STRATEGY_DOCKER_COMPOSE_CLI", &c.Strategy.Docker.ComposeCLI)
	str("STRATEGY_COMMAND_TEMPLATE", &c.Strategy.Command.Template)
	str("STRATEGY_COMMAND_SHELL", &c.Strategy.Command.Shell)
	boolean("STRATEGY_COMMAND_HANDLES_BUILD", &c.Strategy.Command.HandlesBuild)
	boolean("STRATEGY_COMMAND_HANDLES_PULL", &c.Strategy.Command.HandlesPull)
	str("SYNOLOGY_URL", &c.Strategy.Synology.URL)
	str("SYNOLOGY_USERNAME", &c.Strategy.Synology.Username)
	str("SYNOLOGY_PASSWORD", &c.Strategy.Synology.Password)
	str("SYNOLOGY_USERNAME_FILE", &c.Strategy.Synology.UsernameFile)
	str("SYNOLOGY_PASSWORD_FILE", &c.Strategy.Synology.PasswordFile)
	boolean("SYNOLOGY_INSECURE_SKIP_VERIFY", &c.Strategy.Synology.InsecureSkipVerify)
	dur("SYNOLOGY_TIMEOUT", &c.Strategy.Synology.Timeout)
	str("NOTIFY_WEBHOOK_URL", &c.Notify.Webhook.URL)
	str("NOTIFY_WEBHOOK_METHOD", &c.Notify.Webhook.Method)
	str("NOTIFY_WEBHOOK_ON", &c.Notify.Webhook.On)
	str("NOTIFY_WEBHOOK_TEMPLATE", &c.Notify.Webhook.Template)

	if v, ok := lookup(EnvPrefix + "BUILD_PULL"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%sBUILD_PULL: %w", EnvPrefix, err))
		} else {
			c.Build.Pull = &b
		}
	}
	return errors.Join(errs...)
}

// Validate checks the configuration for consistency and resolves secrets
// from files.
func (c *Config) Validate() error {
	var errs []error
	switch c.Strategy.Type {
	case "docker", "command", "synology":
	default:
		errs = append(errs, fmt.Errorf("strategy.type must be docker, command or synology (got %q)", c.Strategy.Type))
	}
	switch c.Build.Mode {
	case "auto", "compose", "api", "command":
	default:
		errs = append(errs, fmt.Errorf("build.mode must be auto, compose, api or command (got %q)", c.Build.Mode))
	}
	if c.Build.Mode == "command" && c.Build.Command == "" {
		errs = append(errs, errors.New("build.command is required when build.mode is command"))
	}
	switch c.Strategy.Docker.ComposeCLI {
	case "auto", "always", "never":
	default:
		errs = append(errs, fmt.Errorf("strategy.docker.compose_cli must be auto, always or never (got %q)", c.Strategy.Docker.ComposeCLI))
	}
	if c.Strategy.Type == "command" && c.Strategy.Command.Template == "" {
		errs = append(errs, errors.New("strategy.command.template is required when strategy.type is command"))
	}
	if c.Strategy.Type == "synology" {
		s := &c.Strategy.Synology
		if s.URL == "" {
			errs = append(errs, errors.New("strategy.synology.url is required"))
		}
		if err := readSecretFile(s.UsernameFile, &s.Username); err != nil {
			errs = append(errs, err)
		}
		if err := readSecretFile(s.PasswordFile, &s.Password); err != nil {
			errs = append(errs, err)
		}
		if s.Username == "" || s.Password == "" {
			errs = append(errs, errors.New("strategy.synology username/password are required (use DOCKWARDEN_SYNOLOGY_USERNAME/PASSWORD env vars or *_file)"))
		}
	}
	switch c.Notify.Webhook.On {
	case "changes", "errors", "always", "":
	default:
		errs = append(errs, fmt.Errorf("notify.webhook.on must be changes, errors or always (got %q)", c.Notify.Webhook.On))
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level must be debug, info, warn or error (got %q)", c.Log.Level))
	}
	return errors.Join(errs...)
}

func readSecretFile(path string, dst *string) error {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read secret file: %w", err)
	}
	*dst = strings.TrimSpace(string(b))
	return nil
}

// BuildPull returns the effective build.pull setting.
func (c *Config) BuildPull() bool { return c.Build.Pull == nil || *c.Build.Pull }

// ShouldRunOnStart returns the effective run_on_start setting.
func (c *Config) ShouldRunOnStart() bool { return c.RunOnStart == nil || *c.RunOnStart }
