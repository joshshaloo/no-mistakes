//go:build !windows

package steps

import (
	"context"
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func newStepShellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", shellenv.OwnOOMScoreScript(command))
}
