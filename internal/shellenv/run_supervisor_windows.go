//go:build windows

package shellenv

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func shellCommandProcessGroup(cmd *exec.Cmd) (int, error) {
	if cmd == nil || cmd.Process == nil {
		return 0, os.ErrProcessDone
	}
	return cmd.Process.Pid, nil
}

func terminateRunProcessGroup(_ context.Context, group int, _ time.Duration) error {
	if group <= 1 || group == os.Getpid() {
		return nil
	}
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(group))
	winproc.Harden(cmd)
	if err := cmd.Run(); err != nil && !isTaskkillAlreadyGone(err) {
		return err
	}
	return nil
}
