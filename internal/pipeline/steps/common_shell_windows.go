//go:build windows

package steps

import (
	"context"
	"os/exec"
	"syscall"
)

func newStepShellCommand(ctx context.Context, command string) *exec.Cmd {
	command = windowsShellCommand(command)
	cmd := exec.CommandContext(ctx, "cmd.exe", "/d", "/s", "/c", command)
	// cmd.exe does not use CommandLineToArgvW quoting: Go's default escaping
	// introduces literal backslashes before embedded quotes. /s strips exactly
	// the enclosing quote pair, leaving the configured command unchanged. The
	// cooperative helper carries this raw line to its target, not to itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `"` + cmd.Path + `" /d /s /c "` + command + `"`,
	}
	return cmd
}
