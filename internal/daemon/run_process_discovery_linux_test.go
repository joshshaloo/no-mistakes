//go:build linux

package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// escapedProcessStep models the gate command this whole mechanism exists for: a
// dashboard or dev server that puts itself in a new session, so terminating the
// command's own process group leaves it running with the worktree as its cwd.
type escapedProcessStep struct {
	pidDir    string
	escapeDir string
	started   chan string
	release   chan struct{}
}

func (s *escapedProcessStep) Name() types.StepName { return types.StepName("escape") }

func (s *escapedProcessStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	pidPath := filepath.Join(s.pidDir, sctx.Run.ID+".escaped.pid")
	if err := launchEscapedRunProcess(sctx.Ctx, sctx.WorkDir, pidPath, s.escapeDir); err != nil {
		return nil, err
	}
	if s.started != nil {
		s.started <- sctx.Run.ID
	}
	if s.release != nil {
		<-s.release
	}
	return &pipeline.StepOutcome{}, nil
}

// TestRunCleanupReapsEscapedProcessDiscoveredInWorktree proves the discovery
// fail-safe actually fires. The launcher's process group is terminated and
// unregistered while the step is still running, so the supervisor holds no
// tracked group for the escapee; only the run-marker scan can find it.
func TestRunCleanupReapsEscapedProcessDiscoveredInWorktree(t *testing.T) {
	requireSetsid(t)
	p, database, repo, head := newRunCleanupFixture(t)
	pidDir := t.TempDir()
	step := &escapedProcessStep{pidDir: pidDir, started: make(chan string, 1), release: make(chan struct{})}
	mgr := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })

	runID, err := mgr.startRun(context.Background(), repo, "target", head, head, "test", nil, "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	<-step.started
	escaped := adoptEscapedProcess(t, filepath.Join(pidDir, runID+".escaped.pid"))
	foreign := startForeignWorktreeProcess(t, p.WorktreeDir(repo.ID, runID), filepath.Join(pidDir, "foreign.pid"))

	close(step.release)
	waitForRunDone(t, mgr, runID)
	waitForWorktreeRemoved(t, p.WorktreeDir(repo.ID, runID))
	waitForTestProcessExit(t, escaped)

	if ok, err := processRunning(foreign); err != nil || !ok {
		t.Fatalf("unmarked foreign process %d in the worktree was killed (ok=%v err=%v)", foreign, ok, err)
	}
}

// TestOrphanWorktreeCleanupReapsEscapedProcess covers the startup trigger the
// intent names: a worktree left behind by a run that is no longer executing
// still has its own marked escapee reaped before removal, while a process
// sharing that directory survives unless it carries this exact run's marker -
// whether it is unmarked or marked for some other run.
func TestOrphanWorktreeCleanupReapsEscapedProcess(t *testing.T) {
	requireSetsid(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	repo, headSHA := setupTestGitRepo(t, p, database, "repo1")
	run, err := database.InsertRun(repo.ID, "old-branch", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, headSHA); err != nil {
		t.Fatal(err)
	}

	pidDir := t.TempDir()
	supervisor := shellenv.NewRunSupervisor(run.ID, worktree)
	ctx := shellenv.WithRunSupervisor(context.Background(), supervisor)
	pidPath := filepath.Join(pidDir, "escaped.pid")
	if err := launchEscapedRunProcess(ctx, worktree, pidPath, ""); err != nil {
		t.Fatalf("launch escaped run process: %v", err)
	}
	escaped := adoptEscapedProcess(t, pidPath)
	foreign := startForeignWorktreeProcess(t, worktree, filepath.Join(pidDir, "foreign.pid"))

	// A process marked for a different run - a live run of a concurrently
	// running daemon that happens to be standing in this directory - must be
	// spared, whichever daemon instance stamped it.
	otherPidPath := filepath.Join(pidDir, "other-run.pid")
	otherCtx := shellenv.WithRunSupervisor(context.Background(), shellenv.NewRunSupervisor("some-other-run", worktree))
	if err := launchEscapedRunProcess(otherCtx, worktree, otherPidPath, ""); err != nil {
		t.Fatalf("launch other run's process: %v", err)
	}
	otherRun := adoptEscapedProcess(t, otherPidPath)

	cleanupOrphanWorktrees(database, p)

	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("orphaned worktree should have been removed, stat err: %v", err)
	}
	waitForTestProcessExit(t, escaped)
	if ok, err := processRunning(foreign); err != nil || !ok {
		t.Fatalf("unmarked foreign process %d in the worktree was killed (ok=%v err=%v)", foreign, ok, err)
	}
	if ok, err := processRunning(otherRun); err != nil || !ok {
		t.Fatalf("process %d marked for a different run was killed (ok=%v err=%v)", otherRun, ok, err)
	}
}

// TestRunCleanupReapsEscapedProcessThatLeftTheWorktree covers the canonical
// daemonization recipe: setsid plus chdir("/"), which dev servers and dashboards
// use so they do not pin a directory. The inherited run marker is the proof of
// ownership, so cleanup must still reap it even though its cwd is nowhere near
// the worktree.
func TestRunCleanupReapsEscapedProcessThatLeftTheWorktree(t *testing.T) {
	requireSetsid(t)
	p, database, repo, head := newRunCleanupFixture(t)
	pidDir := t.TempDir()
	step := &escapedProcessStep{pidDir: pidDir, escapeDir: "/", started: make(chan string, 1), release: make(chan struct{})}
	mgr := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })

	runID, err := mgr.startRun(context.Background(), repo, "target", head, head, "test", nil, "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	<-step.started
	escaped := adoptEscapedProcess(t, filepath.Join(pidDir, runID+".escaped.pid"))
	if cwd := processCwdForTest(t, escaped); cwd != "/" {
		t.Fatalf("escaped process cwd = %q, want %q so the test really covers the chdir case", cwd, "/")
	}
	foreign := startForeignWorktreeProcess(t, p.WorktreeDir(repo.ID, runID), filepath.Join(pidDir, "foreign.pid"))

	close(step.release)
	waitForRunDone(t, mgr, runID)
	waitForWorktreeRemoved(t, p.WorktreeDir(repo.ID, runID))
	waitForTestProcessExit(t, escaped)

	if ok, err := processRunning(foreign); err != nil || !ok {
		t.Fatalf("unmarked foreign process %d was killed (ok=%v err=%v)", foreign, ok, err)
	}
}

func processCwdForTest(t *testing.T, pid int) string {
	t.Helper()
	cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil {
		t.Fatalf("read cwd of %d: %v", pid, err)
	}
	return cwd
}

// launchEscapedRunProcess starts a supervised command whose child calls setsid
// and outlives it, then reaps the launcher's own process group so the survivor
// is no longer reachable through RunSupervisor's registered groups. The child
// inherits the run marker StartShellCommand stamped on the launcher.
func launchEscapedRunProcess(ctx context.Context, workDir, pidPath, escapeDir string) error {
	if escapeDir == "" {
		escapeDir = "."
	}
	script := `setsid sh -c 'cd "$NM_TEST_ESCAPE_DIR" || exit 1; echo $$ > "$NM_TEST_ESCAPED_PIDFILE"; while :; do sleep 1; done' &`
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"NM_TEST_ESCAPED_PIDFILE="+pidPath,
		"NM_TEST_ESCAPE_DIR="+escapeDir,
	)
	shellenv.ConfigureShellCommandForContext(ctx, cmd)
	if err := shellenv.StartShellCommand(cmd); err != nil {
		return err
	}
	err := cmd.Wait()
	if waitErr := waitForPIDFile(pidPath); waitErr != nil {
		shellenv.TerminateShellCommandGroup(cmd)
		return waitErr
	}
	shellenv.TerminateShellCommandGroup(cmd)
	return err
}

func adoptEscapedProcess(t *testing.T, pidPath string) int {
	t.Helper()
	pid := readPID(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if ok, err := processRunning(pid); err != nil || !ok {
		t.Fatalf("escaped process %d did not outlive its launcher (ok=%v err=%v)", pid, ok, err)
	}
	group, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("getpgid for escaped process %d: %v", pid, err)
	}
	if group != pid {
		t.Fatalf("escaped process %d did not lead its own group (pgid=%d)", pid, group)
	}
	return pid
}

func waitForPIDFile(pidPath string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			if pid, convErr := strconv.Atoi(string(bytesTrimSpace(data))); convErr == nil && pid > 0 {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return os.ErrDeadlineExceeded
}

func requireSetsid(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is unavailable; cannot model an escaped process")
	}
}
