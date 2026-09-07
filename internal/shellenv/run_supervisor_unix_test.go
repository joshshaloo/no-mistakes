//go:build unix

package shellenv

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
)

// TestConfigureShellCommandForContext_NoSupervisorKeepsCallerProcessGroup pins
// the CLI/TUI half of the run-ownership boundary: a Git or SCM subprocess
// launched outside a run stays in the caller's foreground process group, so a
// terminal Ctrl-C still reaches it instead of orphaning a network fetch that
// holds gate lock files.
func TestConfigureShellCommandForContext_NoSupervisorKeepsCallerProcessGroup(t *testing.T) {
	supervisor := NewRunSupervisor("run-1", t.TempDir())
	cmd := startProcessGroupProbe(t, context.Background())

	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid {
		t.Fatal("unsupervised command was moved into its own process group")
	}
	group, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	if group != syscall.Getpgrp() {
		t.Fatalf("process group = %d, want caller group %d", group, syscall.Getpgrp())
	}
	if got := len(supervisor.snapshotGroups()); got != 0 {
		t.Fatalf("supervisor tracked %d groups for an unsupervised command", got)
	}
}

func TestConfigureShellCommandForContext_SupervisorIsolatesAndTracksGroup(t *testing.T) {
	supervisor := NewRunSupervisor("run-1", t.TempDir())
	cmd := startProcessGroupProbe(t, WithRunSupervisor(context.Background(), supervisor))

	group, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	if group == syscall.Getpgrp() {
		t.Fatal("supervised command stayed in the caller's process group")
	}
	if group != cmd.Process.Pid {
		t.Fatalf("process group = %d, want leader pid %d", group, cmd.Process.Pid)
	}
	if _, ok := supervisor.snapshotGroups()[group]; !ok {
		t.Fatalf("supervisor did not track group %d", group)
	}
}

func startProcessGroupProbe(t *testing.T, ctx context.Context) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, "sh", "-c", "while :; do sleep 1; done")
	cmd.Dir = t.TempDir()
	ConfigureShellCommandForContext(ctx, cmd)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("start probe: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}
