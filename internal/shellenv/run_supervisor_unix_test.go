//go:build unix

package shellenv

import (
	"context"
	"os/exec"
	"strings"
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

// TestStartShellCommand_StampsRunMarkerOnSupervisedCommand pins the ownership
// proof that survives reparenting: an escaped descendant keeps the inherited
// marker long after its command leader has exited and its ppid chain has been
// rewritten to init, which is what lets worktree discovery recognise it.
func TestStartShellCommand_StampsRunMarkerOnSupervisedCommand(t *testing.T) {
	supervisor := NewRunSupervisor("run-marker", t.TempDir())
	ctx := WithRunSupervisor(context.Background(), supervisor)

	cmd := exec.CommandContext(ctx, "sh", "-c", "env")
	cmd.Dir = t.TempDir()
	ConfigureShellCommandForContext(ctx, cmd)
	out, err := OutputShellCommand(cmd)
	if err != nil {
		t.Fatalf("run probe: %v", err)
	}
	if !strings.Contains(string(out), RunIDEnvVar+"=run-marker") {
		t.Fatalf("child environment missing %s marker:\n%s", RunIDEnvVar, out)
	}
	if !strings.Contains(string(out), DaemonInstanceEnvVar+"="+daemonInstanceID) {
		t.Fatalf("child environment missing %s marker:\n%s", DaemonInstanceEnvVar, out)
	}
}

func TestStartShellCommand_LeavesUnsupervisedCommandUnmarked(t *testing.T) {
	cmd := exec.Command("sh", "-c", "env")
	cmd.Dir = t.TempDir()
	ConfigureShellCommandForContext(context.Background(), cmd)
	out, err := OutputShellCommand(cmd)
	if err != nil {
		t.Fatalf("run probe: %v", err)
	}
	if strings.Contains(string(out), RunIDEnvVar+"=") {
		t.Fatalf("unsupervised child was stamped with a run marker:\n%s", out)
	}
}

func TestRunOwnership_OwnsOnlyMarkedProcesses(t *testing.T) {
	marked := environBlock(RunIDEnvVar+"=run-a", DaemonInstanceEnvVar+"=instance-1", "PATH=/usr/bin")
	otherRun := environBlock(RunIDEnvVar+"=run-b", DaemonInstanceEnvVar+"=instance-1")
	previousInstance := environBlock(RunIDEnvVar+"=run-b", DaemonInstanceEnvVar+"=instance-0")
	foreign := environBlock("PATH=/usr/bin", "SHELL=/bin/sh")

	current := runOwnership{runID: "run-a", instanceID: "instance-1"}
	if !current.owns(marked) {
		t.Error("current run must own its own marked process")
	}
	if current.owns(otherRun) {
		t.Error("current run must not own a sibling run's process")
	}
	if current.owns(previousInstance) {
		t.Error("run-end cleanup must not adopt another run from a previous instance")
	}
	if current.owns(foreign) {
		t.Error("an unmarked foreign process must never be owned")
	}
	if current.owns(nil) {
		t.Error("an unreadable environment must fail closed")
	}

	orphan := runOwnership{runID: "run-a", instanceID: "instance-1", adoptPreviousInstances: true}
	if !orphan.owns(previousInstance) {
		t.Error("startup cleanup must adopt a marked process from a previous daemon instance")
	}
	if orphan.owns(otherRun) {
		t.Error("startup cleanup must not adopt a live sibling run of this instance")
	}
	if orphan.owns(foreign) {
		t.Error("startup cleanup must never adopt an unmarked foreign process")
	}
}

func environBlock(entries ...string) []byte {
	return []byte(strings.Join(entries, "\x00") + "\x00")
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
