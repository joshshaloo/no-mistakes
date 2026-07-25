package daemon

import (
	"context"
	"errors"
	"os"
	"strings"
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

// TestCleanupPreservesTerminalOutcomeWhileRecordingCustodyDiagnostic pins the
// reporting contract: a deliberate `axi abort` must not be rewritten into a
// crash, and a step failure's root cause must stay readable in `axi status`.
// The custody note is recorded alongside them, exactly once across restarts.
func TestCleanupPreservesTerminalOutcomeWhileRecordingCustodyDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    types.RunStatus
		rootError string
	}{
		{name: "cancelled", status: types.RunCancelled, rootError: "aborted by user"},
		{name: "failed", status: types.RunFailed, rootError: "test step failed: 3 tests failing"},
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
			repo, recorded := setupTestGitRepo(t, p, d, "custody-diagnostic-"+tc.name)
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
			if err := os.WriteFile(worktree+"/self-commit.txt", []byte("agent self commit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, worktree, "add", "self-commit.txt")
			gitCmd(t, worktree, "commit", "-m", "unrecorded agent self-commit")
			if err := d.UpdateRunErrorStatus(run.ID, tc.rootError, tc.status); err != nil {
				t.Fatal(err)
			}

			for attempt := 0; attempt < 2; attempt++ {
				if err := cleanupRunWorktree(context.Background(), d, p.RepoDir(repo.ID), worktree, run.ID); !errors.Is(err, errWorktreeRetainedForCustody) {
					t.Fatalf("cleanup attempt %d error = %v, want custody retention", attempt, err)
				}
			}
			reloaded, _ := d.GetRun(run.ID)
			if reloaded.Status != tc.status {
				t.Fatalf("terminal status rewritten to %s, want %s", reloaded.Status, tc.status)
			}
			if reloaded.Error == nil {
				t.Fatal("run error was cleared")
			}
			recordedError := *reloaded.Error
			if !strings.Contains(recordedError, tc.rootError) {
				t.Fatalf("root error discarded: %q", recordedError)
			}
			if !strings.Contains(recordedError, db.RunCustodyDiagnosticMarker) {
				t.Fatalf("custody diagnostic not recorded: %q", recordedError)
			}
			if strings.Count(recordedError, db.RunCustodyDiagnosticMarker) != 1 {
				t.Fatalf("custody diagnostic appended more than once: %q", recordedError)
			}
			// A genuinely different later diagnostic must still land: deduping
			// on the marker prefix would leave `axi status` showing only the
			// first reason while the real one lived in the daemon log.
			later := db.RunCustodyDiagnosticMarker + " run ref names a third head"
			if err := d.RecordRunCustodyDiagnostic(run.ID, later); err != nil {
				t.Fatal(err)
			}
			again, _ := d.GetRun(run.ID)
			if again.Error == nil || !strings.Contains(*again.Error, later) {
				t.Fatalf("different custody diagnostic was swallowed: %v", again.Error)
			}
			if !strings.Contains(*again.Error, tc.rootError) || again.Status != tc.status {
				t.Fatalf("second diagnostic lost root cause or status: %v / %s", again.Error, again.Status)
			}
		})
	}
}

// TestRetirePreservedRunHeadsOnlyRetiresSettledCustody proves preservation refs
// are bounded without ever becoming a code-loss path: only provably settled
// heads are retired, and every uncertain or ambiguous one is retained.
func TestRetirePreservedRunHeadsOnlyRetiresSettledCustody(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, head := setupTestGitRepo(t, p, d, "retire-preserved")
	gate := p.RepoDir(repo.ID)

	newRun := func(branch string) *db.Run {
		t.Helper()
		run, err := d.InsertRun(repo.ID, branch, head, head)
		if err != nil {
			t.Fatal(err)
		}
		if err := git.PinRunHead(context.Background(), gate, run.ID, head); err != nil {
			t.Fatal(err)
		}
		return run
	}
	markTerminal := func(run *db.Run) {
		t.Helper()
		if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
			t.Fatal(err)
		}
	}

	custodyReturned := newRun("custody-returned")
	markTerminal(custodyReturned)
	if err := d.SetRunCustodyReturned(custodyReturned.ID); err != nil {
		t.Fatal(err)
	}

	merged := newRun("merged")
	markTerminal(merged)
	if err := d.UpdateRunPushBinding(merged.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: "fp", Ref: "refs/heads/merged"}); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPRState(merged.ID, "merged"); err != nil {
		t.Fatal(err)
	}

	unresolved := newRun("unresolved")
	markTerminal(unresolved)

	openPR := newRun("open-pr")
	markTerminal(openPR)
	if err := d.UpdateRunPushBinding(openPR.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: "fp", Ref: "refs/heads/open-pr"}); err != nil {
		t.Fatal(err)
	}

	ambiguous := newRun("ambiguous")
	markTerminal(ambiguous)
	if err := d.SetRunCustodyReturned(ambiguous.ID); err != nil {
		t.Fatal(err)
	}
	if err := git.PinExactCommit(context.Background(), gate, git.CrashHeadRef(ambiguous.ID), head); err != nil {
		t.Fatal(err)
	}

	active := newRun("active")

	retired, retained := retirePreservedRunHeads(context.Background(), d, p)
	if retired != 2 || retained != 4 {
		t.Fatalf("retirement counts = %d retired / %d retained, want 2/4", retired, retained)
	}
	for _, run := range []*db.Run{custodyReturned, merged} {
		if exists, _ := git.RefExists(context.Background(), gate, git.RunHeadRef(run.ID)); exists {
			t.Fatalf("settled run %s was not retired", run.Branch)
		}
	}
	for _, run := range []*db.Run{unresolved, openPR, ambiguous, active} {
		if err := git.VerifyExactRef(context.Background(), gate, git.RunHeadRef(run.ID), head); err != nil {
			t.Fatalf("unsettled run %s lost its evidence ref: %v", run.Branch, err)
		}
	}
	if err := git.VerifyExactRef(context.Background(), gate, git.CrashHeadRef(ambiguous.ID), head); err != nil {
		t.Fatalf("crash evidence was retired: %v", err)
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

			// The reported counts must describe what preservation actually did,
			// not restate the parked-run input it was handed.
			stats := preserveActiveRunHeadsBeforeCrashRecovery(d, p, nil)
			want := staleHeadPreservation{Pinned: 1}
			if tc.ambiguous {
				want = staleHeadPreservation{Retained: 1}
			}
			if stats != want {
				t.Fatalf("preservation stats = %#v, want %#v", stats, want)
			}
			if !tc.ambiguous {
				if err := git.VerifyExactRef(context.Background(), p.RepoDir(repo.ID), git.RunHeadRef(run.ID), recorded); err != nil {
					t.Fatalf("recorded head was not pinned before stale failure: %v", err)
				}
			} else if err := git.VerifyExactRef(context.Background(), p.RepoDir(repo.ID), git.CrashHeadRef(run.ID), live); err != nil {
				t.Fatalf("crash candidate was not pinned: %v", err)
			}
			if _, err := d.RecoverStaleRunsExcept("daemon crashed during execution", nil); err != nil {
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
