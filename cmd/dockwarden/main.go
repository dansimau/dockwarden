// Command dockwarden keeps Docker containers up to date: it checks
// registries for newer image digests, rebuilds compose services whose base
// images changed, and restarts containers through a pluggable strategy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dansimau/dockwarden/internal/build"
	"github.com/dansimau/dockwarden/internal/compose"
	"github.com/dansimau/dockwarden/internal/config"
	"github.com/dansimau/dockwarden/internal/dockerapi"
	"github.com/dansimau/dockwarden/internal/engine"
	"github.com/dansimau/dockwarden/internal/execx"
	"github.com/dansimau/dockwarden/internal/notify"
	"github.com/dansimau/dockwarden/internal/registry"
	"github.com/dansimau/dockwarden/internal/state"
	"github.com/dansimau/dockwarden/internal/strategy"
	"github.com/dansimau/dockwarden/internal/synology"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFatal   = 1
	ExitPartial = 2
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func usage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintf(w, `dockwarden %s - keep Docker containers and compose builds up to date

Usage:
  dockwarden [flags]                 run once (or loop with --interval)
  dockwarden synology projects       list DSM Container Manager projects
  dockwarden version                 print version

Flags:
`, version)
	fs.SetOutput(w)
	fs.PrintDefaults()
	fmt.Fprintf(w, `
Exit codes: 0 success, 1 fatal/config error, 2 one or more updates failed,
exit_code_on_updates (if configured) when updates are pending in dry-run or
monitor-only mode. Every YAML key can be overridden with DOCKWARDEN_* env vars.
`)
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dockwarden", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		configPath  = fs.String("config", envOr("DOCKWARDEN_CONFIG", ""), "path to YAML config file")
		dryRun      = fs.Bool("dry-run", false, "report what would be done without pulling, building or restarting")
		monitorOnly = fs.Bool("monitor-only", false, "only report available updates")
		labelEnable = fs.Bool("label-enable", false, "only consider containers labelled com.dockwarden.enable=true")
		interval    = fs.Duration("interval", 0, "run every interval (e.g. 24h); 0 runs once")
		once        = fs.Bool("once", false, "run once even if interval is configured")
		containers  = fs.String("containers", "", "comma-separated container names to consider")
		stratName   = fs.String("strategy", "", "update strategy: docker, command or synology")
		stateFile   = fs.String("state-file", "", "path to the state file")
		logLevel    = fs.String("log-level", "", "debug, info, warn or error")
		logFormat   = fs.String("log-format", "", "text or json")
		cleanup     = fs.Bool("cleanup", false, "remove old images after a successful update")
		showVersion = fs.Bool("version", false, "print version and exit")
		help        = fs.Bool("help", false, "show help")
	)
	fs.BoolVar(help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		usage(stderr, fs)
		return ExitFatal
	}
	if *help {
		usage(stdout, fs)
		return ExitOK
	}
	if *showVersion || (fs.NArg() > 0 && fs.Arg(0) == "version") {
		fmt.Fprintln(stdout, "dockwarden", version)
		return ExitOK
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "config:", err)
		return ExitFatal
	}
	// Flags override config and env.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "dry-run":
			cfg.DryRun = *dryRun
		case "monitor-only":
			cfg.MonitorOnly = *monitorOnly
		case "label-enable":
			cfg.LabelEnable = *labelEnable
		case "interval":
			cfg.Interval = *interval
		case "containers":
			cfg.Containers = splitList(*containers)
		case "strategy":
			cfg.Strategy.Type = *stratName
		case "state-file":
			cfg.StateFile = *stateFile
		case "log-level":
			cfg.Log.Level = *logLevel
		case "log-format":
			cfg.Log.Format = *logFormat
		case "cleanup":
			cfg.Cleanup = *cleanup
		}
	})
	if *once {
		cfg.Interval = 0
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(stderr, "config:", err)
		return ExitFatal
	}
	logger := newLogger(stderr, cfg.Log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if fs.NArg() > 0 && fs.Arg(0) == "synology" {
		return runSynology(ctx, fs.Args()[1:], cfg, logger, stdout, stderr)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unknown command %q\n", fs.Arg(0))
		return ExitFatal
	}

	eng, cleanupFn, err := buildEngine(ctx, cfg, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		return ExitFatal
	}
	defer cleanupFn()

	if cfg.Interval <= 0 {
		return runOnce(ctx, eng, cfg, logger)
	}
	logger.Info("starting scheduler", "interval", cfg.Interval, "run_on_start", cfg.ShouldRunOnStart())
	if cfg.ShouldRunOnStart() {
		runOnce(ctx, eng, cfg, logger)
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return ExitOK
		case <-ticker.C:
			runOnce(ctx, eng, cfg, logger)
		}
	}
}

func runOnce(ctx context.Context, eng *engine.Engine, cfg *config.Config, logger *slog.Logger) int {
	res, err := eng.Run(ctx)
	if err != nil {
		logger.Error("run failed", "error", err)
		return ExitFatal
	}
	return res.ExitCode(cfg.ExitCodeOnUpdates)
}

// buildEngine wires all components from the configuration.
func buildEngine(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*engine.Engine, func(), error) {
	noop := func() {}
	docker, err := dockerapi.New(cfg.Docker.Host)
	if err != nil {
		return nil, noop, fmt.Errorf("docker client: %w", err)
	}
	dockerCfgPath := cfg.Docker.ConfigFile
	if dockerCfgPath == "" {
		dockerCfgPath = registry.DefaultDockerConfigPath()
	}
	creds, err := registry.LoadDockerConfig(dockerCfgPath)
	if err != nil {
		return nil, noop, err
	}
	reg := registry.NewClient(registry.Options{
		Timeout:       cfg.Registry.Timeout,
		SkipTLSVerify: cfg.Registry.SkipTLSVerify,
		Insecure:      cfg.Registry.Insecure,
		Credentials:   creds,
		UserAgent:     "dockwarden/" + version,
	})
	st, err := state.Load(cfg.StateFile)
	if err != nil {
		return nil, noop, err
	}
	runner := execx.Local{Logger: logger}
	pathMap := compose.PathMapper(cfg.Compose.PathMap)

	composeCLI := false
	if cfg.Build.Mode == "auto" || cfg.Build.Mode == "compose" || cfg.Strategy.Docker.ComposeCLI != "never" {
		composeCLI = build.ComposeAvailable(ctx, runner)
		logger.Debug("docker compose CLI availability", "available", composeCLI)
	}

	var builder build.Builder
	switch cfg.Build.Mode {
	case "compose":
		if !composeCLI {
			return nil, noop, errors.New("build.mode is compose but `docker compose` is not available")
		}
		builder = &build.ComposeCLI{Runner: runner, Logger: logger, PathMap: pathMap}
	case "api":
		builder = &build.API{Client: docker, Logger: logger}
	case "command":
		b, err := build.NewCommand(cfg.Build.Command, cfg.Strategy.Command.Shell, runner, logger)
		if err != nil {
			return nil, noop, err
		}
		builder = b
	default: // auto
		if composeCLI {
			builder = &build.ComposeCLI{Runner: runner, Logger: logger, PathMap: pathMap}
		} else {
			builder = &build.API{Client: docker, Logger: logger}
		}
	}

	var strat strategy.Strategy
	cleanupFn := noop
	switch cfg.Strategy.Type {
	case "docker":
		useCLI := cfg.Strategy.Docker.ComposeCLI == "always" || (cfg.Strategy.Docker.ComposeCLI == "auto" && composeCLI)
		if cfg.Strategy.Docker.ComposeCLI == "always" && !composeCLI {
			return nil, noop, errors.New("strategy.docker.compose_cli is always but `docker compose` is not available")
		}
		strat = &strategy.Docker{Client: docker, Runner: runner, Logger: logger, StopTimeout: cfg.StopTimeout, UseComposeCLI: useCLI, PathMap: pathMap}
	case "command":
		c := cfg.Strategy.Command
		s, err := strategy.NewCommand(c.Template, c.Shell, c.HandlesBuild, c.HandlesPull, c.Timeout, runner, logger)
		if err != nil {
			return nil, noop, err
		}
		strat = s
	case "synology":
		client, err := newSynologyClient(cfg, logger)
		if err != nil {
			return nil, noop, err
		}
		s := &strategy.Synology{Client: client, Logger: logger, ProjectMap: cfg.Strategy.Synology.ProjectMap}
		strat = s
		cleanupFn = func() { _ = s.Close(context.Background()) }
	}

	notifier, err := notify.New(notify.Options{
		URL: cfg.Notify.Webhook.URL, Method: cfg.Notify.Webhook.Method, Headers: cfg.Notify.Webhook.Headers,
		Template: cfg.Notify.Webhook.Template, On: cfg.Notify.Webhook.On, Timeout: cfg.Notify.Webhook.Timeout, Logger: logger,
	})
	if err != nil {
		return nil, noop, err
	}

	self, _ := os.Hostname()
	logger.Info("dockwarden starting", "version", version, "strategy", strat.Name(), "builder", builder.Name(), "dry_run", cfg.DryRun, "monitor_only", cfg.MonitorOnly, "label_enable", cfg.LabelEnable)
	eng := engine.New(engine.Options{
		Docker:         docker,
		Registry:       reg,
		Credentials:    creds,
		State:          st,
		Strategy:       strat,
		Builder:        builder,
		Notifier:       notifier,
		Logger:         logger,
		DryRun:         cfg.DryRun,
		MonitorOnly:    cfg.MonitorOnly,
		LabelEnable:    cfg.LabelEnable,
		IncludeStopped: cfg.IncludeStopped,
		Containers:     cfg.Containers,
		Cleanup:        cfg.Cleanup,
		BuildPull:      cfg.BuildPull(),
		BuildNoCache:   cfg.Build.NoCache,
		PathMap:        pathMap,
		SelfID:         self,
	})
	return eng, func() { cleanupFn(); docker.Close() }, nil
}

func newSynologyClient(cfg *config.Config, logger *slog.Logger) (*synology.Client, error) {
	s := cfg.Strategy.Synology
	return synology.New(synology.Options{
		URL:                s.URL,
		Username:           s.Username,
		Password:           s.Password,
		InsecureSkipVerify: s.InsecureSkipVerify,
		Timeout:            s.Timeout,
		Logger:             logger,
	})
}

// runSynology implements `dockwarden synology projects|containers`.
func runSynology(ctx context.Context, args []string, cfg *config.Config, logger *slog.Logger, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "projects" && args[0] != "containers") {
		fmt.Fprintln(stderr, "usage: dockwarden synology projects|containers")
		return ExitFatal
	}
	cfg.Strategy.Type = "synology"
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(stderr, "config:", err)
		return ExitFatal
	}
	client, err := newSynologyClient(cfg, logger)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitFatal
	}
	if err := client.Login(ctx); err != nil {
		fmt.Fprintln(stderr, "login:", err)
		return ExitFatal
	}
	defer client.Logout(context.Background()) //nolint:errcheck
	switch args[0] {
	case "projects":
		ps, err := client.ListProjects(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return ExitFatal
		}
		fmt.Fprintf(stdout, "%-38s %-24s %-10s %s\n", "ID", "NAME", "STATUS", "PATH")
		for _, p := range ps {
			fmt.Fprintf(stdout, "%-38s %-24s %-10s %s\n", p.ID, p.Name, p.Status, p.Path)
		}
	case "containers":
		cs, err := client.ListContainers(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return ExitFatal
		}
		fmt.Fprintf(stdout, "%-30s %-40s %s\n", "NAME", "IMAGE", "STATUS")
		for _, c := range cs {
			fmt.Fprintf(stdout, "%-30s %-40s %s\n", c.Name, c.Image, c.Status)
		}
	}
	return ExitOK
}

func newLogger(w io.Writer, cfg config.LogConfig) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(cfg.Format, "json") {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
