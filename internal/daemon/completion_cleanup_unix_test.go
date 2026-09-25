//go:build unix

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type unrecordedHeadStep struct{}

func (*unrecordedHeadStep) Name() types.StepName { return types.StepLint }
func (*unrecordedHeadStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, "unrecorded.txt"), []byte("preserve this work\n"), 0o644); err != nil {
		return nil, err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "unrecorded.txt"); err != nil {
		return nil, err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "unrecorded head"); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}

func TestStartupTerminalPRCompletionReapsOwnedProcesses(t *testing.T) {
	for _, removeWorktreeFirst := range []bool{false, true} {
		name := "worktree present"
		if removeWorktreeFirst {
			name = "worktree already absent"
		}
		t.Run(name, func(t *testing.T) {
			p, database, repo, run, workDir := newTerminalPRCleanupFixture(t)
			pidDir := t.TempDir()
			ownedPIDPath := filepath.Join(pidDir, "owned.pid")
			ownedSupervisor := shellenv.NewRunSupervisor(run.ID, workDir)
			t.Cleanup(func() { _ = ownedSupervisor.Terminate(context.Background()) })
			ownedCtx := shellenv.WithRunSupervisor(context.Background(), ownedSupervisor)
			if err := startRunOwnedBackgroundChild(ownedCtx, workDir, ownedPIDPath); err != nil {
				t.Fatal(err)
			}
			ownedPID := readPID(t, ownedPIDPath)

			siblingPIDPath := filepath.Join(pidDir, "sibling.pid")
			siblingSupervisor := shellenv.NewRunSupervisor("sibling-run", workDir)
			t.Cleanup(func() { _ = siblingSupervisor.Terminate(context.Background()) })
			siblingCtx := shellenv.WithRunSupervisor(context.Background(), siblingSupervisor)
			if err := startRunOwnedBackgroundChild(siblingCtx, workDir, siblingPIDPath); err != nil {
				t.Fatal(err)
			}
			siblingPID := readPID(t, siblingPIDPath)
			foreignPID := startForeignWorktreeProcess(t, workDir, filepath.Join(pidDir, "foreign.pid"))

			if removeWorktreeFirst {
				if err := git.WorktreeRemove(context.Background(), p.RepoDir(repo.ID), workDir); err != nil {
					t.Fatal(err)
				}
			}

			mgr := NewRunManager(database, p, nil)
			events, unsubscribe := mgr.Subscribe(run.ID)
			defer unsubscribe()
			if count := reconcileTerminalPRRuns(database, p, mgr); count != 1 {
				t.Fatalf("reconciled runs = %d, want 1", count)
			}
			event := <-events
			if event.Type != ipc.EventRunCompleted || event.Status == nil || *event.Status != string(types.RunCompleted) {
				t.Fatalf("completion event = %#v", event)
			}
			if alive, err := processRunning(ownedPID); err != nil || alive {
				t.Fatalf("completion published with owned process %d alive=%v err=%v", ownedPID, alive, err)
			}
			for label, pid := range map[string]int{"sibling": siblingPID, "unmarked": foreignPID} {
				if alive, err := processRunning(pid); err != nil || !alive {
					t.Fatalf("%s process %d was signalled: alive=%v err=%v", label, pid, alive, err)
				}
			}
			got, err := database.GetRun(run.ID)
			if err != nil || got.Status != types.RunCompleted {
				t.Fatalf("completed run = %#v, %v", got, err)
			}
		})
	}
}

func newTerminalPRCleanupFixture(t *testing.T) (*paths.Paths, *db.DB, *db.Repo, *db.Run, string) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repo, head := setupTestGitRepo(t, p, database, "terminal-pr-process-cleanup")
	run, err := database.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.ObserveRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(ci.ID); err != nil {
		t.Fatal(err)
	}
	workDir := p.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), workDir, head); err != nil {
		t.Fatal(err)
	}
	return p, database, repo, run, workDir
}

func TestRunCompletionFailsWhenCleanupRetainsWorktree(t *testing.T) {
	p, database, repo, head := newRunCleanupFixture(t)
	mgr := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{&unrecordedHeadStep{}} })
	runID, err := mgr.startRun(context.Background(), repo, "target", head, head, "test", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForRunDone(t, mgr, runID)
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, errWorktreeRetainedForCustody.Error()) {
		t.Fatalf("retained run = %+v, want failed with custody diagnostic", run)
	}
	workDir := p.WorktreeDir(repo.ID, runID)
	if data, err := os.ReadFile(filepath.Join(workDir, "unrecorded.txt")); err != nil || string(data) != "preserve this work\n" {
		t.Fatalf("retained work = %q, %v", data, err)
	}
	if pinned, err := git.ResolveRef(context.Background(), p.RepoDir(repo.ID), git.RunHeadRef(runID)); err != nil || pinned != head {
		t.Fatalf("recorded head = %q, %v", pinned, err)
	}
	if crash, err := git.ResolveRef(context.Background(), p.RepoDir(repo.ID), git.CrashHeadRef(runID)); err != nil || crash == head || crash == "" {
		t.Fatalf("crash head = %q, %v", crash, err)
	}
}
