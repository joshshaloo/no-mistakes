//go:build unix

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type runProcessStep struct {
	pidDir  string
	started chan string
	release chan struct{}
}

func (s *runProcessStep) Name() types.StepName { return types.StepName("process") }

func (s *runProcessStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	pidPath := filepath.Join(s.pidDir, sctx.Run.ID+".pid")
	if err := startRunOwnedBackgroundChild(sctx.Ctx, sctx.WorkDir, pidPath); err != nil {
		return nil, err
	}
	if s.started != nil {
		s.started <- sctx.Run.ID
	}
	if sctx.Run.Branch == "sibling" {
		<-sctx.Ctx.Done()
		return nil, sctx.Ctx.Err()
	}
	if s.release != nil {
		<-s.release
	}
	return &pipeline.StepOutcome{}, nil
}

func startRunOwnedBackgroundChild(ctx context.Context, workDir, pidPath string) error {
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return err
	}
	script := fmt.Sprintf("(while :; do sleep 1; done) & echo $! > %s", shellQuoteForTest(pidPath))
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = workDir
	shellenv.ConfigureShellCommandForContext(ctx, cmd)
	if err := shellenv.StartShellCommand(cmd); err != nil {
		return err
	}
	// The fake step intentionally does not call TerminateShellCommandGroup: this
	// models a gate command that leaves a background server behind after its
	// leader exits. The run supervisor, not the step, owns final cleanup.
	return cmd.Wait()
}

func TestRunCleanupKillsBackgroundChildBeforeRemovingWorktree(t *testing.T) {
	p, database, repo, head := newRunCleanupFixture(t)
	pidDir := t.TempDir()
	step := &runProcessStep{pidDir: pidDir, started: make(chan string, 1), release: make(chan struct{})}
	mgr := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })

	runID, err := mgr.startRun(context.Background(), repo, "target", head, head, "test", nil, "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	<-step.started
	pid := readPID(t, filepath.Join(pidDir, runID+".pid"))
	if ok, err := processRunning(pid); err != nil || !ok {
		t.Fatalf("background child %d not running before cleanup (ok=%v err=%v)", pid, ok, err)
	}

	close(step.release)
	waitForRunDone(t, mgr, runID)
	waitForWorktreeRemoved(t, p.WorktreeDir(repo.ID, runID))
	waitForTestProcessExit(t, pid)
}

func TestRunCleanupDoesNotKillDaemonOrSiblingRun(t *testing.T) {
	p, database, repo, head := newRunCleanupFixture(t)
	pidDir := t.TempDir()
	step := &runProcessStep{pidDir: pidDir, started: make(chan string, 2), release: make(chan struct{})}
	mgr := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })

	fakeDaemon := startFakeDaemonProcess(t)

	siblingID, err := mgr.startRun(context.Background(), repo, "sibling", head, head, "test", nil, "")
	if err != nil {
		t.Fatalf("start sibling run: %v", err)
	}
	waitForStartedRun(t, step.started, siblingID)
	siblingPID := readPID(t, filepath.Join(pidDir, siblingID+".pid"))

	targetID, err := mgr.startRun(context.Background(), repo, "target", head, head, "test", nil, "")
	if err != nil {
		t.Fatalf("start target run: %v", err)
	}
	waitForStartedRun(t, step.started, targetID)
	targetPID := readPID(t, filepath.Join(pidDir, targetID+".pid"))
	close(step.release)

	waitForRunDone(t, mgr, targetID)
	waitForWorktreeRemoved(t, p.WorktreeDir(repo.ID, targetID))
	waitForTestProcessExit(t, targetPID)

	if ok, err := processRunning(fakeDaemon.Process.Pid); err != nil || !ok {
		t.Fatalf("fake daemon process was killed by target cleanup (ok=%v err=%v)", ok, err)
	}
	if ok, err := processRunning(siblingPID); err != nil || !ok {
		t.Fatalf("sibling run child was killed by target cleanup (ok=%v err=%v)", ok, err)
	}
	if run, err := database.GetRun(siblingID); err != nil || run == nil || run.Status != types.RunRunning {
		t.Fatalf("sibling run status = %#v err=%v, want running", run, err)
	}

	if err := mgr.HandleCancel(siblingID); err != nil {
		t.Fatalf("cancel sibling: %v", err)
	}
	waitForRunDone(t, mgr, siblingID)
	waitForWorktreeRemoved(t, p.WorktreeDir(repo.ID, siblingID))
	waitForTestProcessExit(t, siblingPID)
}

func newRunCleanupFixture(t *testing.T) (*paths.Paths, *db.DB, *db.Repo, string) {
	t.Helper()
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	config.EnsureDefaultGlobalConfig(p.ConfigFile())
	configYAML := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repo, head := setupTestGitRepo(t, p, database, "repo1")
	return p, database, repo, head
}

func waitForRunDone(t *testing.T, mgr *RunManager, runID string) {
	t.Helper()
	mgr.mu.Lock()
	done := mgr.dones[runID]
	mgr.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("run %s did not finish", runID)
	}
}

func waitForStartedRun(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case got := <-ch:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("run %s did not start its background child", want)
		}
	}
}

func waitForWorktreeRemoved(t *testing.T, worktree string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(worktree); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worktree still exists: %s", worktree)
}

func waitForTestProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ok, err := processRunning(pid)
		if err == nil && !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d is still running", pid)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file %s: %v", path, err)
	}
	pid, err := strconv.Atoi(string(bytesTrimSpace(data)))
	if err != nil {
		t.Fatalf("parse pid file %s: %v", path, err)
	}
	return pid
}

func startFakeDaemonProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", "while :; do sleep 1; done")
	cmd.Dir = t.TempDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake daemon process: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func bytesTrimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\n' || b[0] == '\t' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 {
		c := b[len(b)-1]
		if c != ' ' && c != '\n' && c != '\t' && c != '\r' {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}
