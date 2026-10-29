package scm

import (
	"context"
	"errors"
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// Command keeps provider CLI execution on the run-supervised start/reap
// boundary, including upstream call sites that use exec.Cmd-style methods.
// Cmd is exposed only for helpers that already use shellenv execution wrappers.
type Command struct{ *exec.Cmd }

func (c *Command) Run() error { return shellenv.RunShellCommand(c.Cmd) }
func (c *Command) Output() ([]byte, error) {
	// exec.Cmd.Output populates ExitError.Stderr when the caller did not
	// supply a writer. Preserve that contract with bounded diagnostics.
	var stderr *commandStderr
	if c.Stderr == nil {
		stderr = &commandStderr{}
		c.Stderr = stderr
	}
	out, err := shellenv.OutputShellCommand(c.Cmd)
	var exitErr *exec.ExitError
	if stderr != nil && errors.As(err, &exitErr) {
		exitErr.Stderr = stderr.data
	}
	return out, err
}

type commandStderr struct{ data []byte }

func (s *commandStderr) Write(p []byte) (int, error) {
	n := len(p)
	const limit = 32 * 1024
	if len(p) > limit-len(s.data) {
		p = p[:limit-len(s.data)]
	}
	s.data = append(s.data, p...)
	return n, nil
}
func (c *Command) CombinedOutput() ([]byte, error) { return shellenv.CombinedOutputShellCommand(c.Cmd) }

type CommandFactory func(context.Context, string, ...string) *Command

func SuperviseCommands(factory func(context.Context, string, ...string) *exec.Cmd) CommandFactory {
	return func(ctx context.Context, name string, args ...string) *Command {
		cmd := factory(ctx, name, args...)
		shellenv.ConfigureShellCommandForContext(ctx, cmd)
		return &Command{Cmd: cmd}
	}
}
