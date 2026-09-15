//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRunCleanupKillsEscapedConfiguredCommandDashboard drives the real binary,
// the real daemon, and a real gate push with a configured test command that
// leaves a dashboard behind. The dashboard deliberately escapes every ancestry
// signal a run could use to find it - setsid gives it a session of its own,
// chdir("/") moves it out of the worktree, and its leader exits so it is
// reparented to init - so only the inherited run marker can prove it belongs to
// the run. When the run ends the worktree must be gone, the dashboard must be
// dead, and the daemon must still be serving.
func TestRunCleanupKillsEscapedConfiguredCommandDashboard(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	stateDir := t.TempDir()
	pidFile := filepath.Join(stateDir, "dashboard.pid")
	markerFile := filepath.Join(stateDir, "dashboard.marker")
	commandName := "nm-e2e-dashboard"
	script := fmt.Sprintf(`#!/bin/sh
# Model a configured gate command that starts a background dashboard and exits.
# setsid + cd / + an exiting leader strips process-group, cwd, and ppid
# ancestry, leaving the inherited run marker as the only proof of ownership.
setsid sh -c 'cd /; echo $$ > %[1]s; printenv NO_MISTAKES_RUN_ID > %[2]s; exec sleep 600' >/dev/null 2>&1 </dev/null &
i=0
while [ ! -s %[1]s ] && [ "$i" -lt 100 ]; do
  sleep 0.1
  i=$((i + 1))
done
echo "dashboard started with pid $(cat %[1]s)"
exit 0
`, pidFile, markerFile)
	commandPath := filepath.Join(h.BinDir, commandName)
	if err := os.WriteFile(commandPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write dashboard command: %v", err)
	}

	branch := "leaky-dashboard"
	h.CommitChange(branch, ".no-mistakes.yaml",
		"allow_repo_commands: true\ncommands:\n  test: "+commandName+"\n",
		"configure a test command that leaves a dashboard running")
	h.PushToGate(branch)

	active := h.WaitForRunRunning(branch, 30*time.Second)
	worktree := filepath.Join(h.NMHome, "worktrees", h.repoID(), active.ID)

	dashboardPID := waitForPIDFile(t, pidFile, 60*time.Second)
	markedRun := strings.TrimSpace(readFileForTest(t, markerFile))
	t.Logf("dashboard pid=%d NO_MISTAKES_RUN_ID=%q run_id=%q", dashboardPID, markedRun, active.ID)
	if markedRun != active.ID {
		t.Fatalf("dashboard did not inherit the run marker: got %q want %q", markedRun, active.ID)
	}
	if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", dashboardPID)); err == nil {
		t.Logf("dashboard cwd=%s (outside worktree %s)", cwd, worktree)
	}

	daemonPID, err := daemon.ReadPID(paths.WithRoot(h.NMHome))
	if err != nil || daemonPID <= 0 {
		t.Fatalf("read daemon pid: %v (pid=%d)", err, daemonPID)
	}

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}

	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after run cleanup (stat err=%v)", worktree, err)
	}
	if alive := waitForProcessExit(dashboardPID, 15*time.Second); alive {
		t.Fatalf("escaped dashboard pid %d survived run cleanup", dashboardPID)
	}
	if !processAlive(daemonPID) {
		t.Fatalf("daemon pid %d was killed by run cleanup", daemonPID)
	}
	t.Logf("after cleanup: worktree removed, dashboard pid %d dead, daemon pid %d alive", dashboardPID, daemonPID)

	// The dashboard must have been reaped through marker discovery, not merely
	// as a member of a registered command group it had already left.
	reapLine := findDaemonLogLine(t, h, "reaping run process that escaped its command group", active.ID)
	t.Logf("daemon log: %s", reapLine)
}

func findDaemonLogLine(t *testing.T, h *Harness, message, runID string) string {
	t.Helper()
	logPath := filepath.Join(h.NMHome, "logs", "daemon.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read daemon log %s: %v", logPath, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, message) && strings.Contains(line, runID) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("daemon log %s has no %q line for run %s", logPath, message, runID)
	return ""
}

func waitForPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if data, err := os.ReadFile(path); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dashboard pid file %s never appeared", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !processAlive(pid) {
			return false
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
}
