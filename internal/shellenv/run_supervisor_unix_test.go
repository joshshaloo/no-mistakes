//go:build unix

package shellenv

import (
	"context"
	"os/exec"
	"slices"
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

// TestRunOwnership_OwnsOnlyMarkedProcesses pins the single ownership predicate.
// Carrying this run's marker is the only thing that authorizes a kill: no other
// signal - another run's marker, a shared working directory - may ever stand in
// for it, because several daemons with different NM_HOME roots can be live at
// once and one of them may still be executing that other run.
func TestRunOwnership_OwnsOnlyMarkedProcesses(t *testing.T) {
	ownRun := environBlock(RunIDEnvVar+"=run-a", "PATH=/usr/bin")
	siblingRun := environBlock(RunIDEnvVar+"=run-b", "PATH=/usr/bin")
	emptyRunID := environBlock(RunIDEnvVar+"=", "PATH=/usr/bin")
	foreign := environBlock("PATH=/usr/bin", "SHELL=/bin/sh")

	current := runOwnership{runID: "run-a"}
	if !current.ownsRun(ownRun) {
		t.Error("current run must own its own marked process")
	}
	if current.ownsRun(siblingRun) {
		t.Error("another run's process must be spared, whichever daemon stamped it")
	}
	if current.ownsRun(emptyRunID) {
		t.Error("an empty run ID must never match")
	}
	if current.ownsRun(foreign) {
		t.Error("an unmarked foreign process must never be owned")
	}
	if current.ownsRun(nil) {
		t.Error("an unreadable environment must fail closed")
	}
	if (runOwnership{}).ownsRun(ownRun) {
		t.Error("a supervisor without a run ID must own nothing")
	}
}

// TestWithRunMarkers_ReplacesInheritedMarker pins that a marker already present
// in the environment is removed rather than shadowed. A daemon running inside
// another run - this repository dogfoods its own pipeline - would otherwise hand
// its own marker down, and cleanup would attribute the escapee to the wrong run.
func TestWithRunMarkers_ReplacesInheritedMarker(t *testing.T) {
	supervisor := NewRunSupervisor("run-inner", t.TempDir())
	marked := supervisor.WithRunMarkers([]string{
		RunIDEnvVar + "=run-outer",
		"PATH=/usr/bin",
	})

	var runIDs []string
	for _, entry := range marked {
		if hasEnvName(entry, RunIDEnvVar) {
			runIDs = append(runIDs, entry)
		}
	}
	if len(runIDs) != 1 {
		t.Fatalf("expected exactly one %s entry, got %v", RunIDEnvVar, runIDs)
	}
	if runIDs[0] != RunIDEnvVar+"=run-inner" {
		t.Fatalf("%s = %q, want the supervisor's own run", RunIDEnvVar, runIDs[0])
	}
	if !slices.Contains(marked, "PATH=/usr/bin") {
		t.Errorf("unrelated environment entries must be preserved: %v", marked)
	}
}

func TestWithRunMarkers_LeavesEnvAloneWithoutARun(t *testing.T) {
	env := []string{"PATH=/usr/bin"}
	if got := (*RunSupervisor)(nil).WithRunMarkers(env); !slices.Equal(got, env) {
		t.Errorf("nil supervisor changed the environment: %v", got)
	}
	if got := NewRunSupervisor("", t.TempDir()).WithRunMarkers(env); !slices.Equal(got, env) {
		t.Errorf("supervisor without a run ID changed the environment: %v", got)
	}
}

func TestPathInside(t *testing.T) {
	if !pathInside("/a/b", "/a/b") {
		t.Error("a path is inside itself")
	}
	if !pathInside("/a/b/c", "/a/b") {
		t.Error("a descendant must be inside")
	}
	if pathInside("/a/bc", "/a/b") {
		t.Error("a sibling sharing a name prefix must not be inside")
	}
	if pathInside("/a", "/a/b") {
		t.Error("an ancestor must not be inside")
	}
	if pathInside("", "/a/b") || pathInside("/a/b", "") {
		t.Error("an empty path or root must not match")
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
