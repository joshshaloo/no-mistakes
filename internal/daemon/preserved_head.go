package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

var errWorktreeRetainedForCustody = errors.New("worktree retained for custody recovery")

// preserveWorktreeHead archives every head whose worktree reference cleanup
// removes. It deliberately does not promote authority or refuse merely because
// live and recorded heads differ: custody follows upstream's verified-terminal
// lifecycle, while immutable archives make its cleanup safe for unlanded work.
// Legacy run-head/crash-head refs are left intact, never overwritten or retired.
func preserveWorktreeHead(ctx context.Context, gateDir, workDir string, run *db.Run) error {
	if run == nil {
		return errors.New("cannot archive a worktree without a run identity")
	}
	live, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		return fmt.Errorf("resolve worktree HEAD for run %s: %w", run.ID, err)
	}
	for _, head := range []string{live, strings.TrimSpace(run.HeadSHA)} {
		if head == "" {
			continue
		}
		if err := archiveCleanupHead(ctx, gateDir, run.ID, head); err != nil {
			return err
		}
	}
	return nil
}

func archiveCleanupHead(ctx context.Context, gateDir, runID, head string) error {
	if _, err := git.ResolveExactCommit(ctx, gateDir, head); err != nil {
		return fmt.Errorf("verify cleanup head %s: %w", head, err)
	}
	ref := git.ArchiveHeadRef(runID, head)
	if err := custody.PreserveRecoveryAnchor(ctx, gateDir, ref, head); err != nil {
		return fmt.Errorf("archive cleanup head %s: %w", head, err)
	}
	return git.VerifyExactRef(ctx, gateDir, ref, head)
}

func cleanupRunWorktree(ctx context.Context, d *db.DB, gateDir, workDir, runID string) error {
	return cleanupRunWorktreeWithSupervisor(ctx, d, gateDir, workDir, runID, shellenv.NewRunSupervisor(runID, workDir))
}

func cleanupRunWorktreeWithSupervisor(ctx context.Context, d *db.DB, gateDir, workDir, runID string, supervisor *shellenv.RunSupervisor) error {
	run, err := d.GetRun(runID)
	if err != nil {
		return fmt.Errorf("load run before worktree cleanup: %w", err)
	}
	if supervisor == nil {
		supervisor = shellenv.NewRunSupervisor(runID, workDir)
	}
	if err := supervisor.Terminate(ctx); err != nil {
		return err
	}
	if reason := protectedPathCleanupReason(d, run); reason != "" {
		return recordCustodyFailure(d, runID, errors.New(reason))
	}
	if _, err := os.Stat(workDir); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect worktree before cleanup: %w", err)
		}
		if run == nil {
			return nil
		}
		if err := archiveCleanupHead(ctx, gateDir, run.ID, run.HeadSHA); err != nil {
			return recordCustodyFailure(d, runID, err)
		}
		return nil
	}
	if run == nil {
		// A no-row, non-Git directory cannot contain commits. A no-row Git
		// worktree still gets an archive before removal, using its directory ID.
		if _, err := os.Stat(filepath.Join(workDir, ".git")); os.IsNotExist(err) {
			return os.RemoveAll(workDir)
		}
		run = &db.Run{ID: runID}
	}
	if err := preserveWorktreeHead(ctx, gateDir, workDir, run); err != nil {
		return recordCustodyFailure(d, runID, err)
	}
	return git.WorktreeRemove(ctx, gateDir, workDir)
}

func recordCustodyFailure(d *db.DB, runID string, cause error) error {
	retained := fmt.Errorf("%w: %w", errWorktreeRetainedForCustody, cause)
	if err := d.RecordRunCustodyDiagnostic(runID, fmt.Sprintf("%s %v", db.RunCustodyDiagnosticMarker, cause)); err != nil {
		return errors.Join(retained, fmt.Errorf("record cleanup preservation failure: %w", err))
	}
	return retained
}
