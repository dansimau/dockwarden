package build

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"text/template"

	"github.com/dansimau/dockwarden/internal/execx"
)

// Command builds by running a user template through the shell.
type Command struct {
	Template *template.Template
	Shell    string
	Runner   execx.Runner
	Logger   *slog.Logger
}

// CommandData is the template context for build commands.
type CommandData struct {
	Project     string
	Service     string
	WorkingDir  string
	ConfigFiles string
	Context     string
	Dockerfile  string
	Tag         string
	BaseImages  string
	Pull        bool
	NoCache     bool
}

// NewCommand parses the template.
func NewCommand(tmpl, shell string, runner execx.Runner, l *slog.Logger) (*Command, error) {
	t, err := template.New("build").Funcs(template.FuncMap{"shellquote": shellQuote}).Parse(tmpl)
	if err != nil {
		return nil, fmt.Errorf("parse build.command: %w", err)
	}
	if shell == "" {
		shell = "/bin/sh"
	}
	return &Command{Template: t, Shell: shell, Runner: runner, Logger: l}, nil
}

func (b *Command) Name() string { return "command" }

// Build implements Builder.
func (b *Command) Build(ctx context.Context, req Request) error {
	data := CommandData{
		Project:     req.Info.Project,
		Service:     req.Info.Service,
		WorkingDir:  req.Info.WorkingDir,
		ConfigFiles: strings.Join(req.Info.ConfigFiles, ","),
		Tag:         req.Tag,
		BaseImages:  strings.Join(req.BaseImages, ","),
		Pull:        req.Pull,
		NoCache:     req.NoCache,
	}
	if req.Spec != nil {
		data.Context = req.Spec.Context
		data.Dockerfile = req.Spec.Dockerfile
	}
	var buf bytes.Buffer
	if err := b.Template.Execute(&buf, data); err != nil {
		return fmt.Errorf("render build.command: %w", err)
	}
	logger(b.Logger).Info("building via command", "project", req.Info.Project, "service", req.Info.Service, "command", buf.String())
	_, err := b.Runner.Run(ctx, execx.Command{Name: b.Shell, Args: []string{"-c", buf.String()}})
	return err
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
