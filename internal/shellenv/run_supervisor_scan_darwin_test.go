//go:build darwin

package shellenv

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestDiscoverRunProcessesDarwinFindsDescendantInWorktree(t *testing.T) {
	requireDarwinLsof(t)
	workDir := t.TempDir()
	cmd := startDarwinDiscoveryFixture(t, workDir)
	group, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}

	found := waitForDarwinDiscovery(t, runOwnership{runID: "run-darwin"}, workDir)
	if len(found) != 1 {
		t.Fatalf("discoverRunProcesses found %d process groups, want 1: %#v", len(found), found)
	}
	if found[0].pid != cmd.Process.Pid {
		t.Fatalf("discovered pid = %d, want fixture pid %d", found[0].pid, cmd.Process.Pid)
	}
	if found[0].group != group {
		t.Fatalf("discovered pgid = %d, want %d", found[0].group, group)
	}
	if !found[0].inWorktree || !pathInside(found[0].cwd, workDir) {
		t.Fatalf("discovered cwd = %q inWorktree=%v, want inside %q", found[0].cwd, found[0].inWorktree, workDir)
	}
}

func TestDiscoverRunProcessesDarwinRequiresDaemonDescendant(t *testing.T) {
	requireDarwinLsof(t)
	workDir := t.TempDir()
	cmd := startDarwinDiscoveryFixture(t, workDir)

	oldCurrentPID := darwinCurrentPID
	darwinCurrentPID = func() int { return cmd.Process.Pid + 1000000 }
	t.Cleanup(func() { darwinCurrentPID = oldCurrentPID })

	found, err := discoverRunProcesses(runOwnership{runID: "run-darwin"}, workDir)
	if err != nil {
		t.Fatalf("discoverRunProcesses: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("discoverRunProcesses found non-descendant groups: %#v", found)
	}
}

func TestRunSupervisorTerminateDarwinKillsDiscoveredDescendant(t *testing.T) {
	requireDarwinLsof(t)
	workDir := t.TempDir()
	cmd := startDarwinDiscoveryFixture(t, workDir)

	supervisor := NewRunSupervisor("run-darwin", workDir)
	if err := supervisor.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	waitForDarwinCommandExit(t, cmd)
}

func startDarwinDiscoveryFixture(t *testing.T, workDir string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "sh", "-c", "while :; do sleep 1; done")
	cmd.Dir = workDir
	cmd.Env = NewRunSupervisor("run-darwin", workDir).WithRunMarkers(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fixture: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func waitForDarwinDiscovery(t *testing.T, owner runOwnership, workDir string) []discoveredProcess {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []discoveredProcess
	for time.Now().Before(deadline) {
		found, err := discoverRunProcesses(owner, workDir)
		if err != nil {
			t.Fatalf("discoverRunProcesses: %v", err)
		}
		if len(found) > 0 {
			return found
		}
		last = found
		time.Sleep(100 * time.Millisecond)
	}
	return last
}

func waitForDarwinCommandExit(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return
	case <-time.After(5 * time.Second):
		t.Fatalf("process %d is still running", cmd.Process.Pid)
	}
}

func requireDarwinLsof(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skipf("lsof unavailable: %v", err)
	}
}
