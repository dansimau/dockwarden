// Package execx runs external commands with logging and a swappable Runner
// for tests.
package execx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// Command describes a command to run.
type Command struct {
	Name string
	Args []string
	Dir  string
	Env  []string // appended to the current environment
	// Quiet suppresses streaming output to the logger (output is still captured).
	Quiet bool
}

// String renders the command for logs.
func (c Command) String() string {
	parts := append([]string{c.Name}, c.Args...)
	return strings.Join(parts, " ")
}

// Runner executes commands.
type Runner interface {
	Run(ctx context.Context, cmd Command) (string, error)
}

// Local runs commands on the local machine.
type Local struct {
	Logger *slog.Logger
}

// Run implements Runner. Output (stdout+stderr) is returned and streamed to
// the logger at debug level.
func (l Local) Run(ctx context.Context, cmd Command) (string, error) {
	c := exec.CommandContext(ctx, cmd.Name, cmd.Args...)
	c.Dir = cmd.Dir
	c.Env = append(os.Environ(), cmd.Env...)
	var buf bytes.Buffer
	var w io.Writer = &buf
	if !cmd.Quiet && l.Logger != nil {
		w = io.MultiWriter(&buf, &logWriter{l: l.Logger})
	}
	c.Stdout = w
	c.Stderr = w
	err := c.Run()
	out := buf.String()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", cmd.Name, err, lastLines(out, 20))
	}
	return out, nil
}

// LookPath reports whether a binary is available.
func LookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

type logWriter struct {
	l   *slog.Logger
	buf []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if line != "" {
			w.l.Debug(line)
		}
	}
	return len(p), nil
}

// Fake is a Runner for tests that records commands.
type Fake struct {
	Commands []Command
	// Handler optionally produces output/error per command.
	Handler func(cmd Command) (string, error)
}

// Run implements Runner.
func (f *Fake) Run(ctx context.Context, cmd Command) (string, error) {
	f.Commands = append(f.Commands, cmd)
	if f.Handler != nil {
		return f.Handler(cmd)
	}
	return "", nil
}

// Strings returns the recorded commands rendered as strings.
func (f *Fake) Strings() []string {
	out := make([]string, len(f.Commands))
	for i, c := range f.Commands {
		out[i] = c.String()
	}
	return out
}
