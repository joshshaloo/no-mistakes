package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/runcompletion"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

func reconcileTerminalPRRuns(d *db.DB, p *paths.Paths, mgr *RunManager) int {
	candidates, err := d.TerminalPRCompletionCandidates()
	if err != nil {
		slog.Error("failed to list terminal PR runs", "error", err)
		return 0
	}
	completed := 0
	for _, run := range candidates {
		workDir := worktrees.RecordedDir(p, run.WorktreePath(), run.RepoID, run.ID)
		preserveRunHead(d, workDir, run)
		cleanup := func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return cleanupRunWorktreeWithSupervisor(ctx, d, p.RepoDir(run.RepoID), workDir, run.ID, shellenv.NewRunSupervisor(run.ID, workDir))
		}
		finalizer := runcompletion.Finalizer{DB: d, Cleanup: cleanup, OnEvent: mgr.broadcast, CompleteTerminalPRStep: true}
		if err := finalizer.Finalize(context.Background(), run, nil); err != nil {
			slog.Error("failed to reconcile terminal PR run", "run_id", run.ID, "error", err)
			continue
		}
		completed++
	}
	return completed
}
