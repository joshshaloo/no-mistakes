package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var (
	errAmbiguousWorktreeHead = errors.New("ambiguous live worktree head")
	// errWorktreeRetainedForCustody marks the class of cleanup failure where
	// the recorded head could not be safely pinned, so the worktree is the last
	// reference to pipeline work. Callers must distinguish it from an ordinary
	// `git worktree remove` failure, whose cause is the removal, not pinning.
	errWorktreeRetainedForCustody = errors.New("worktree retained for custody recovery")
)

// preserveWorktreeHead proves that the durable run head is pinned before a
// terminal worktree can lose its last reference. A differing live HEAD is
// separately pinned as crash evidence and reported as ambiguous; callers must
// retain the worktree and must never promote either value automatically.
func preserveWorktreeHead(ctx context.Context, gateDir, workDir string, run *db.Run) error {
	if run == nil {
		return nil
	}
	live, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		return fmt.Errorf("resolve worktree HEAD for run %s: %w", run.ID, err)
	}
	recorded := strings.TrimSpace(run.HeadSHA)
	runRef := git.RunHeadRef(run.ID)
	runRefExists, refErr := git.RefExists(ctx, gateDir, runRef)
	if refErr != nil {
		return fmt.Errorf("verify existing run head ref for run %s: %w", run.ID, refErr)
	}
	if runRefExists {
		existing, resolveErr := git.ResolveRef(ctx, gateDir, runRef)
		if resolveErr != nil {
			return fmt.Errorf("resolve existing run head ref for run %s: %w", run.ID, resolveErr)
		}
		if existing != recorded {
			// A Git publication succeeded but durable DB authority did not. Keep
			// the exact candidate under the crash ref before restoring RunHeadRef
			// to recorded authority. A third live head remains protected by the
			// retained worktree.
			candidateErr := git.PinExactCommit(ctx, gateDir, git.CrashHeadRef(run.ID), existing)
			recordedErr := git.PinRunHead(ctx, gateDir, run.ID, recorded)
			if candidateErr != nil || recordedErr != nil {
				return fmt.Errorf("%w for run %s: run ref %s differs from recorded %s; candidate pin error: %v; recorded pin error: %v", errAmbiguousWorktreeHead, run.ID, existing, recorded, candidateErr, recordedErr)
			}
			return fmt.Errorf("%w for run %s: run ref %s differed from recorded %s and live worktree is %s; candidate preserved at %s, authority restored at %s, and worktree retained", errAmbiguousWorktreeHead, run.ID, existing, recorded, live, git.CrashHeadRef(run.ID), runRef)
		}
	}
	if live != recorded {
		// Keep durable authority and the unexpected live candidate under
		// separate exact refs. The worktree remains present as an independent
		// final reference until an operator resolves the ambiguity.
		recordedErr := git.PinRunHead(ctx, gateDir, run.ID, recorded)
		candidateRef := git.CrashHeadRef(run.ID)
		candidateErr := git.PinExactCommit(ctx, gateDir, candidateRef, live)
		if recordedErr != nil || candidateErr != nil {
			return fmt.Errorf("%w for run %s: live %s differs from recorded %s; recorded pin error: %v; crash candidate pin error: %v", errAmbiguousWorktreeHead, run.ID, live, recorded, recordedErr, candidateErr)
		}
		return fmt.Errorf("%w for run %s: live %s differs from recorded %s; authority preserved at %s, candidate preserved at %s, and worktree must be retained", errAmbiguousWorktreeHead, run.ID, live, recorded, runRef, candidateRef)
	}
	if err := git.PinRunHead(ctx, gateDir, run.ID, recorded); err != nil {
		return fmt.Errorf("pin recorded head %s for run %s before worktree removal: %w", recorded, run.ID, err)
	}
	if err := git.VerifyExactRef(ctx, gateDir, git.RunHeadRef(run.ID), recorded); err != nil {
		return fmt.Errorf("verify recorded head pin for run %s before worktree removal: %w", run.ID, err)
	}
	return nil
}

// cleanupRunWorktree is the only normal run-lifecycle removal boundary. A
// preservation failure is terminal and recoverable: the worktree is retained
// and the run records the custody diagnostic alongside - never instead of - the
// terminal status and root error that already explain why the run ended. No
// fallback os.RemoveAll is attempted.
func cleanupRunWorktree(ctx context.Context, d *db.DB, gateDir, workDir, runID string) error {
	return cleanupRunWorktreeWithSupervisor(ctx, d, gateDir, workDir, runID, nil)
}

func cleanupRunWorktreeWithSupervisor(ctx context.Context, d *db.DB, gateDir, workDir, runID string, supervisor *shellenv.RunSupervisor) error {
	run, err := d.GetRun(runID)
	if err != nil {
		return fmt.Errorf("load run before worktree cleanup: %w", err)
	}
	if err := supervisor.Terminate(ctx); err != nil {
		return err
	}
	if _, err := os.Stat(workDir); err != nil {
		if os.IsNotExist(err) {
			if run == nil {
				return nil
			}
			if verifyErr := git.VerifyExactRef(ctx, gateDir, git.RunHeadRef(run.ID), strings.TrimSpace(run.HeadSHA)); verifyErr != nil {
				return recordCustodyFailure(d, run.ID, fmt.Errorf("worktree is absent and exact recorded head custody cannot be verified: %w", verifyErr))
			}
			return nil
		}
		return fmt.Errorf("inspect worktree before cleanup: %w", err)
	}
	if run != nil {
		if preserveErr := preserveWorktreeHead(ctx, gateDir, workDir, run); preserveErr != nil {
			return recordCustodyFailure(d, run.ID, preserveErr)
		}
	}
	if err := git.WorktreeRemove(ctx, gateDir, workDir); err != nil {
		return err
	}
	return nil
}

func recordCustodyFailure(d *db.DB, runID string, cause error) error {
	retained := fmt.Errorf("%w: %w", errWorktreeRetainedForCustody, cause)
	msg := fmt.Sprintf("%s %v", db.RunCustodyDiagnosticMarker, cause)
	if err := d.RecordRunCustodyDiagnostic(runID, msg); err != nil {
		return errors.Join(retained, fmt.Errorf("record cleanup preservation failure: %w", err))
	}
	return retained
}

// staleHeadPreservation counts what a startup preservation pass actually did,
// so the startup log reports real outcomes instead of restating its input.
type staleHeadPreservation struct {
	// Pinned counts stale runs whose recorded head was exactly preserved, so
	// their worktree is safe for cleanup to remove.
	Pinned int
	// Retained counts stale runs whose head could not be exactly preserved or
	// was ambiguous, so cleanup must keep the worktree as final evidence.
	Retained int
}

// preserveActiveRunHeadsBeforeCrashRecovery closes the startup ordering gap:
// every stale worktree is inspected and pinned before RecoverStaleRuns can make
// it terminal. Preservation failures are still marked terminal so they surface
// as crashed, but the guarded cleanup independently retains the worktree and
// records the stronger custody diagnostic. Parked runs are skipped: they are
// already excluded from generic crash recovery by their caller.
func preserveActiveRunHeadsBeforeCrashRecovery(d *db.DB, p *paths.Paths, parked map[string]struct{}) staleHeadPreservation {
	stats := staleHeadPreservation{}
	active, err := d.GetActiveRuns()
	if err != nil {
		slog.Error("failed to load active runs before crash-head preservation", "error", err)
		return stats
	}
	ctx := context.Background()
	for _, run := range active {
		if _, isParked := parked[run.ID]; isParked {
			continue
		}
		workDir := p.WorktreeDir(run.RepoID, run.ID)
		gateDir := p.RepoDir(run.RepoID)
		if _, statErr := os.Stat(workDir); statErr != nil {
			if os.IsNotExist(statErr) {
				// There is no worktree reference left for startup cleanup to
				// remove. Generic stale-run recovery may still mark the row
				// failed; recovery will independently require an exact anchor.
				continue
			}
			stats.Retained++
			slog.Error("stale run worktree could not be inspected; terminal cleanup will retain it", "run_id", run.ID, "error", statErr)
			continue
		}
		err := preserveWorktreeHead(ctx, gateDir, workDir, run)
		if err == nil {
			stats.Pinned++
			continue
		}
		stats.Retained++
		if errors.Is(err, errAmbiguousWorktreeHead) {
			slog.Error("stale run has ambiguous live worktree head; both heads retained", "run_id", run.ID, "error", err)
			continue
		}
		slog.Error("failed to preserve stale run head; terminal cleanup will retain the worktree", "run_id", run.ID, "error", err)
	}
	return stats
}

// retirePreservedRunHeads is the bounded counterpart to preservation: without
// it every run-owned ref lives forever and `git gc` in the managed gate can
// never reclaim a single pipeline object. A ref is retired only when its run is
// terminal, no run is active on that branch, it carries no crash candidate, it
// still matches durable authority exactly, and either custody was durably
// returned to the operator or the exact head reached the configured push target
// and its pull request merged. Crash evidence, resolution archives, and every
// uncertain head are always retained, and deletion is compare-and-swap on the
// exact observed value.
func retirePreservedRunHeads(ctx context.Context, d *db.DB, p *paths.Paths) (retired int, retained int) {
	repos, err := d.GetRepos()
	if err != nil {
		slog.Error("failed to load repos before preservation-ref retirement", "error", err)
		return 0, 0
	}
	for _, repo := range repos {
		gateDir := p.RepoDir(repo.ID)
		if _, statErr := os.Stat(gateDir); statErr != nil {
			continue
		}
		refs, refErr := git.PreservedHeads(ctx, gateDir)
		if refErr != nil {
			slog.Warn("failed to read preservation refs; retiring nothing for this repo", "repo_id", repo.ID, "error", refErr)
			continue
		}
		for ref, head := range refs {
			runID, ok := git.RunIDFromRunHeadRef(ref)
			if !ok {
				continue
			}
			run, runErr := d.GetRun(runID)
			if runErr != nil || run == nil || !settledPreservedRunHead(d, refs, run, head) {
				retained++
				continue
			}
			if delErr := git.DeletePrivateRefExactly(ctx, gateDir, ref, head); delErr != nil {
				slog.Warn("failed to retire settled preservation ref", "ref", ref, "error", delErr)
				retained++
				continue
			}
			retired++
		}
	}
	return retired, retained
}

func settledPreservedRunHead(d *db.DB, refs map[string]string, run *db.Run, head string) bool {
	if !terminalRunStatus(run.Status) || run.HeadSHA != head {
		return false
	}
	if _, ambiguous := refs[git.CrashHeadRef(run.ID)]; ambiguous {
		return false
	}
	active, err := d.GetActiveRun(run.RepoID, run.Branch)
	if err != nil || active != nil {
		return false
	}
	if run.CustodyReturnedAt != nil {
		return true
	}
	if run.LastPushedSHA == nil || *run.LastPushedSHA != run.HeadSHA {
		return false
	}
	return run.PRState != nil && strings.EqualFold(strings.TrimSpace(*run.PRState), "merged")
}

func terminalRunStatus(status types.RunStatus) bool {
	switch status {
	case types.RunCompleted, types.RunFailed, types.RunCancelled:
		return true
	default:
		return false
	}
}
