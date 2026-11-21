package strategy

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/dansimau/dockwarden/internal/execx"
)

// Command runs a user-configured shell command template for each target.
type Command struct {
	Template     *template.Template
	Shell        string
	Runner       execx.Runner
	Logger       *slog.Logger
	Timeout      time.Duration
	handlesBuild bool
	handlesPull  bool
}

// TemplateData is what the command template is rendered with.
type TemplateData struct {
	Kind          string
	ContainerID   string
	ContainerName string
	Image         string
	OldImageID    string
	Project       string
	Service       string
	WorkingDir    string
	ConfigFiles   string
	BaseImages    string
	// Labels of the container.
	Labels map[string]string
}

// NewCommand parses the template.
func NewCommand(tmpl, shell string, handlesBuild, handlesPull bool, timeout time.Duration, runner execx.Runner, logger *slog.Logger) (*Command, error) {
	t, err := template.New("command").Funcs(template.FuncMap{
		"shellquote": ShellQuote,
	}).Parse(tmpl)
	if err != nil {
		return nil, fmt.Errorf("parse strategy.command.template: %w", err)
	}
	if shell == "" {
		shell = "/bin/sh"
	}
	return &Command{Template: t, Shell: shell, Runner: runner, Logger: logger, Timeout: timeout, handlesBuild: handlesBuild, handlesPull: handlesPull}, nil
}

func (c *Command) Name() string       { return "command" }
func (c *Command) HandlesBuild() bool { return c.handlesBuild }
func (c *Command) HandlesPull() bool  { return c.handlesPull }

// Render produces the command line for a target.
func (c *Command) Render(t *Target) (string, error) {
	data := TemplateData{
		Kind:          string(t.Kind),
		ContainerID:   t.ContainerID,
		ContainerName: t.ContainerName,
		Image:         t.Image,
		OldImageID:    t.OldImageID,
		Project:       t.Project(),
		Service:       t.Service(),
		WorkingDir:    t.WorkingDir(),
		ConfigFiles:   t.ConfigFiles(),
		BaseImages:    strings.Join(t.BaseImages, ","),
	}
	if t.Inspect.Config != nil {
		data.Labels = t.Inspect.Config.Labels
	}
	var buf bytes.Buffer
	if err := c.Template.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render command template: %w", err)
	}
	return buf.String(), nil
}

// Update implements Strategy.
func (c *Command) Update(ctx context.Context, t *Target) error {
	cmdline, err := c.Render(t)
	if err != nil {
		return err
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	c.Logger.Info("running update command", "container", t.ContainerName, "command", cmdline)
	_, err = c.Runner.Run(ctx, execx.Command{Name: c.Shell, Args: []string{"-c", cmdline}})
	return err
}

// ShellQuote quotes s for POSIX shells.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
