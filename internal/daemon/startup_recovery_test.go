package daemon

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRecoverOnStartup_DoesNotDeleteActiveRunWorktree is the regression test
// for the second half of the duplicate-daemon wedge: startup cleanup used to
// remove every worktree directory under the shared root with no check of the
// owning run's status, so a duplicate daemon's cleanup could delete the
// checkout out from under a pipeline that was still actively running in it
// (observed live as "chdir .../worktrees/...: no such file or directory").
// A worktree whose run row is pending or running must survive cleanup.
//
// This exercises cleanupOrphanWorktrees directly (rather than the full
// recoverOnStartup) because RecoverStaleRuns, which runs first in
// production, unconditionally marks every pending/running run failed - so by
// design there is no pending/running row left by the time cleanup runs in
// the normal single-daemon path. Testing cleanupOrphanWorktrees in isolation
// verifies its DB-aware skip logic as defense in depth, independent of
// whatever recovery step runs before it.
func TestRecoverOnStartup_DoesNotDeleteActiveRunWorktree(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, headSHA := setupTestGitRepo(t, p, d, "repo1")
	activeRun, err := d.InsertRun(repo.ID, "feature", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if activeRun.Status != types.RunPending {
		t.Fatalf("expected new run to default to pending, got %s", activeRun.Status)
	}

	activeWT := p.WorktreeDir(repo.ID, activeRun.ID)
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), activeWT, headSHA); err != nil {
		t.Fatal(err)
	}

	// A terminal run's real managed worktree, for contrast: cleanup should pin
	// its exact recorded head and then remove it.
	terminalRun, err := d.InsertRun(repo.ID, "old-branch", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(terminalRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	terminalWT := p.WorktreeDir(repo.ID, terminalRun.ID)
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), terminalWT, headSHA); err != nil {
		t.Fatal(err)
	}

	cleanupOrphanWorktrees(d, p)

	if _, err := os.Stat(activeWT); err != nil {
		t.Fatalf("active run worktree must survive cleanup, got: %v", err)
	}
	if _, err := os.Stat(terminalWT); !os.IsNotExist(err) {
		t.Fatalf("terminal run worktree should have been cleaned up, stat err: %v", err)
	}

	got, err := d.GetRun(activeRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunPending {
		t.Fatalf("expected active run to remain pending, got %s", got.Status)
	}
}

func TestCleanupRunWorktreePinsRecordedHeadBeforeRemoval(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, head := setupTestGitRepo(t, p, d, "cleanup-pin")
	run, err := d.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, head); err != nil {
		t.Fatal(err)
	}

	if err := cleanupRunWorktree(context.Background(), d, p.RepoDir(repo.ID), worktree, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree was not removed: %v", err)
	}
	if err := git.VerifyExactRef(context.Background(), p.RepoDir(repo.ID), git.RunHeadRef(run.ID), head); err != nil {
		t.Fatalf("recorded head not pinned before removal: %v", err)
	}
}

func TestCleanupRunWorktreeRetainsAmbiguousLiveHead(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, recorded := setupTestGitRepo(t, p, d, "cleanup-ambiguous")
	run, err := d.InsertRun(repo.ID, "feature", recorded, recorded)
	if err != nil {
		t.Fatal(err)
	}
	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, recorded); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, worktree, "config", "user.name", "test")
	gitCmd(t, worktree, "config", "user.email", "test@example.com")
	if err := os.WriteFile(worktree+"/ambiguous.txt", []byte("pipeline candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, worktree, "add", "ambiguous.txt")
	gitCmd(t, worktree, "commit", "-m", "unrecorded pipeline candidate")
	live := gitOutput(t, worktree, "rev-parse", "HEAD")
	// Model Git publication succeeding before the run-head DB write failed.
	if err := git.PinRunHead(context.Background(), p.RepoDir(repo.ID), run.ID, live); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	if err := cleanupRunWorktree(context.Background(), d, p.RepoDir(repo.ID), worktree, run.ID); err == nil || !errors.Is(err, errAmbiguousWorktreeHead) {
		t.Fatalf("cleanup error = %v, want ambiguous refusal", err)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("ambiguous worktree was removed: %v", err)
	}
	if err := git.VerifyExactRef(context.Background(), p.RepoDir(repo.ID), git.CrashHeadRef(run.ID), live); err != nil {
		t.Fatalf("ambiguous live head was not separately pinned: %v", err)
	}
	if err := git.VerifyExactRef(context.Background(), p.RepoDir(repo.ID), git.RunHeadRef(run.ID), recorded); err != nil {
		t.Fatalf("recorded authority was not separately pinned: %v", err)
	}
	reloaded, _ := d.GetRun(run.ID)
	if reloaded.HeadSHA != recorded {
		t.Fatalf("ambiguous live head was silently promoted: %s", reloaded.HeadSHA)
	}
}

func TestCrashRecoveryPinsRecordedHeadBeforeFailureAndRetainsAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ambiguous bool
	}{
		{name: "recorded head"},
		{name: "ambiguous live head", ambiguous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, recorded := setupTestGitRepo(t, p, d, "startup-"+tc.name)
			run, err := d.InsertRun(repo.ID, "feature", recorded, recorded)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			worktree := p.WorktreeDir(repo.ID, run.ID)
			if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, recorded); err != nil {
				t.Fatal(err)
			}
			live := recorded
			if tc.ambiguous {
				gitCmd(t, worktree, "config", "user.name", "test")
				gitCmd(t, worktree, "config", "user.email", "test@example.com")
				if err := os.WriteFile(worktree+"/candidate.txt", []byte("candidate\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, worktree, "add", "candidate.txt")
				gitCmd(t, worktree, "commit", "-m", "crash candidate")
				live = gitOutput(t, worktree, "rev-parse", "HEAD")
			}

			preserved := preserveActiveRunHeadsBeforeCrashRecovery(d, p, nil)
			if _, retainedActive := preserved[run.ID]; retainedActive {
				t.Fatal("verifiable crash evidence unnecessarily retained the row active")
			}
			if !tc.ambiguous {
				if err := git.VerifyExactRef(context.Background(), p.RepoDir(repo.ID), git.RunHeadRef(run.ID), recorded); err != nil {
					t.Fatalf("recorded head was not pinned before stale failure: %v", err)
				}
			} else if err := git.VerifyExactRef(context.Background(), p.RepoDir(repo.ID), git.CrashHeadRef(run.ID), live); err != nil {
				t.Fatalf("crash candidate was not pinned: %v", err)
			}
			if _, err := d.RecoverStaleRunsExcept("daemon crashed during execution", preserved); err != nil {
				t.Fatal(err)
			}
			cleanupOrphanWorktrees(d, p)
			if tc.ambiguous {
				if _, err := os.Stat(worktree); err != nil {
					t.Fatalf("ambiguous crash worktree was removed: %v", err)
				}
			} else if _, err := os.Stat(worktree); !os.IsNotExist(err) {
				t.Fatalf("exactly pinned crash worktree was not removed: %v", err)
			}
			reloaded, _ := d.GetRun(run.ID)
			if reloaded.Status != types.RunFailed || reloaded.HeadSHA != recorded {
				t.Fatalf("recovered run = %#v", reloaded)
			}
		})
	}
}

// TestRunWithOptions_RequiresSingletonLockBeforeRecovery proves the ordering
// the fix depends on: when another process already holds the singleton lock
// for this root, RunWithOptions must fail before ever calling
// RecoverStaleRuns, so a duplicate daemon can never mark a live daemon's
// active runs as crashed.
func TestRunWithOptions_RequiresSingletonLockBeforeRecovery(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, err := d.InsertRepoWithID("repo1", "/nonexistent/work", "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate another live daemon already owning this root.
	lock, err := acquireSingletonLock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if err := RunWithOptions(p, d, nil); err == nil {
		t.Fatal("expected RunWithOptions to fail while the singleton lock is held elsewhere")
	} else if !errors.Is(err, ErrSingletonLockHeld) {
		t.Fatalf("expected ErrSingletonLockHeld, got %v", err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunPending {
		t.Fatalf("recovery must not have run: expected run to remain pending, got %s", got.Status)
	}
}
