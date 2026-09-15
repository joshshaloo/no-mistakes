//go:build unix

package shellenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func shellCommandProcessGroup(cmd *exec.Cmd) (int, error) {
	if cmd == nil || cmd.Process == nil {
		return 0, os.ErrProcessDone
	}
	return syscall.Getpgid(cmd.Process.Pid)
}

func terminateRunProcessGroup(ctx context.Context, group int, grace time.Duration) error {
	if group <= 1 || group == syscall.Getpgrp() {
		return nil
	}
	if err := syscall.Kill(-group, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if err := waitForProcessGroupExit(ctx, group, grace); err == nil {
		return nil
	} else if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	// SIGKILL is the terminal enforcement point. Do not fail worktree cleanup if
	// the kernel needs a moment to reap zombies that no longer hold cwd or ports.
	_ = waitForProcessGroupExit(ctx, group, grace)
	return nil
}

func waitForProcessGroupExit(ctx context.Context, group int, grace time.Duration) error {
	if group <= 1 {
		return nil
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if groupGone(group) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if groupGone(group) {
				return nil
			}
			return context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

func groupGone(group int) bool {
	err := syscall.Kill(-group, 0)
	return errors.Is(err, syscall.ESRCH)
}
