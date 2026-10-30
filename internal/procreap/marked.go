package procreap

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// SweepMarkedRunWorktrees is the fork's production cleanup boundary. A cwd or
// descendant relationship is not proof of ownership: only the inherited run
// marker authorizes signalling. This also catches workers that left the cwd.
// The legacy cwd matcher remains available for diagnostics and its pure tests.
func SweepMarkedRunWorktrees(_ string, wts []Worktree, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seen := make(map[string]bool, len(wts))
	for _, wt := range wts {
		if strings.TrimSpace(wt.RunID) == "" || seen[wt.RunID] {
			continue
		}
		seen[wt.RunID] = true
		if err := shellenv.NewRunSupervisor(wt.RunID, wt.Dir).Terminate(ctx); err != nil {
			slog.Warn("run-marker process cleanup failed", "run_id", wt.RunID, "reason", reason, "error", err)
		}
	}
}
