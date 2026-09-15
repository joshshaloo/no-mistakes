//go:build !unix && !windows

package shellenv

import (
	"context"
	"os"
	"os/exec"
	"time"
)

func shellCommandProcessGroup(cmd *exec.Cmd) (int, error) {
	if cmd == nil || cmd.Process == nil {
		return 0, os.ErrProcessDone
	}
	return cmd.Process.Pid, nil
}

func terminateRunProcessGroup(context.Context, int, time.Duration) error { return nil }
