//go:build unix

package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

type unrecordedHeadStep struct{ breakArchive bool }

func (*unrecordedHeadStep) Name() types.StepName { return types.StepLint }
func (s *unrecordedHeadStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, "unrecorded.txt"), []byte("preserve this work\n"), 0o644); err != nil {
		return nil, err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "unrecorded.txt"); err != nil {
		return nil, err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "unrecorded head"); err != nil {
		return nil, err
	}
	if s.breakArchive {
		head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
		if err != nil {
			return nil, err
		}
		if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "symbolic-ref", git.ArchiveHeadRef(sctx.Run.ID, head), "refs/heads/main"); err != nil {
			return nil, err
		}
	}
	return &pipeline.StepOutcome{}, nil
}

func TestStartupTerminalPRCompletionReapsOwnedProcesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("restart-time marker discovery is implemented through /proc on Linux")
	}
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
				if err := git.PinRunHead(context.Background(), p.RepoDir(repo.ID), run.ID, run.HeadSHA); err != nil {
					t.Fatal(err)
				}
				if err := git.WorktreeRemove(context.Background(), p.RepoDir(repo.ID), workDir); err != nil {
					t.Fatal(err)
				}
			}

			mgr := NewRunManager(database, p, nil)
			events, err := mgr.Subscribe(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer events.Close()
			if _, ok := events.Next(context.Background()); !ok {
				t.Fatal("missing initial reconciliation gap")
			}
			if count := reconcileTerminalPRRuns(database, p, mgr); count != 1 {
				t.Fatalf("reconciled runs = %d, want 1", count)
			}
			event, ok := events.Next(context.Background())
			if !ok {
				t.Fatal("completion event missing")
			}
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

func TestRunCompletionWaitsForArchivedCleanup(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint("archive_failure=", broken), func(t *testing.T) {
			p, database, repo, head := newRunCleanupFixture(t)
			mgr := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{&unrecordedHeadStep{breakArchive: broken}} })
			runID, err := mgr.startRun(context.Background(), repo, "target", head, head, "test", nil, "", "", false, "")
			if err != nil {
				t.Fatal(err)
			}
			waitForRunDone(t, mgr, runID)
			run, err := database.GetRun(runID)
			if err != nil {
				t.Fatal(err)
			}
			workDir := p.WorktreeDir(repo.ID, runID)
			if broken {
				if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, errWorktreeRetainedForCustody.Error()) {
					t.Fatalf("retained run = %+v", run)
				}
				if data, err := os.ReadFile(filepath.Join(workDir, "unrecorded.txt")); err != nil || string(data) != "preserve this work\n" {
					t.Fatalf("retained work = %q, %v", data, err)
				}
			} else {
				if run.Status != types.RunCompleted {
					t.Fatalf("archived run = %+v", run)
				}
				if _, err := os.Stat(workDir); !os.IsNotExist(err) {
					t.Fatalf("completed with retained worktree: %v", err)
				}
				ref := git.ArchiveHeadRef(runID, run.HeadSHA)
				if pinned, err := git.ResolveRef(context.Background(), p.RepoDir(repo.ID), ref); err != nil || pinned != run.HeadSHA || pinned == head {
					t.Fatalf("archive = %q, %v", pinned, err)
				}
			}
		})
	}
}
