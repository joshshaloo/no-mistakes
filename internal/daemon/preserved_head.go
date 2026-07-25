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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var errAmbiguousWorktreeHead = errors.New("ambiguous live worktree head")

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
// preservation failure is terminal and recoverable: the worktree is retained,
// the run records why, and no fallback os.RemoveAll is attempted.
func cleanupRunWorktree(ctx context.Context, d *db.DB, gateDir, workDir, runID string) error {
	run, err := d.GetRun(runID)
	if err != nil {
		return fmt.Errorf("load run before worktree cleanup: %w", err)
	}
	if run != nil {
		if preserveErr := preserveWorktreeHead(ctx, gateDir, workDir, run); preserveErr != nil {
			msg := fmt.Sprintf("worktree retained for custody recovery: %v", preserveErr)
			if dbErr := d.UpdateRunErrorStatus(run.ID, msg, types.RunFailed); dbErr != nil {
				return errors.Join(fmt.Errorf("worktree retained for custody recovery: %w", preserveErr), fmt.Errorf("record cleanup preservation failure: %w", dbErr))
			}
			return fmt.Errorf("worktree retained for custody recovery: %w", preserveErr)
		}
	}
	if err := git.WorktreeRemove(ctx, gateDir, workDir); err != nil {
		return err
	}
	return nil
}

// preserveActiveRunHeadsBeforeCrashRecovery closes the startup ordering gap:
// every stale worktree is inspected and pinned before RecoverStaleRuns can make
// it terminal. Preservation failures are still marked terminal so they surface
// as crashed, but the guarded cleanup independently retains the worktree and
// records the stronger custody error. Parked runs remain the only active rows
// excluded from generic crash recovery.
func preserveActiveRunHeadsBeforeCrashRecovery(d *db.DB, p *paths.Paths, alreadyPreserved map[string]struct{}) map[string]struct{} {
	active, err := d.GetActiveRuns()
	if err != nil {
		slog.Error("failed to load active runs before crash-head preservation", "error", err)
		return alreadyPreserved
	}
	preserved := make(map[string]struct{}, len(alreadyPreserved)+len(active))
	for id := range alreadyPreserved {
		preserved[id] = struct{}{}
	}
	ctx := context.Background()
	for _, run := range active {
		if _, parked := alreadyPreserved[run.ID]; parked {
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
			slog.Error("stale run worktree could not be inspected; terminal cleanup will retain it", "run_id", run.ID, "error", statErr)
			continue
		}
		err := preserveWorktreeHead(ctx, gateDir, workDir, run)
		if err == nil {
			continue
		}
		if errors.Is(err, errAmbiguousWorktreeHead) {
			slog.Error("stale run has ambiguous live worktree head; both heads retained", "run_id", run.ID, "error", err)
			continue
		}
		slog.Error("failed to preserve stale run head; terminal cleanup will retain the worktree", "run_id", run.ID, "error", err)
	}
	return preserved
}
