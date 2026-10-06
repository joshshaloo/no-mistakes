package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// completionCleanup is shared by the executor's success barrier and the
// manager's deferred failure/panic cleanup. Execute and Resume used to publish
// completion before that defer reaped processes, preserved the head and removed
// the worktree. Moving only the event (or closing subscribers later) cannot fix
// the window for DB/IPC readers. Success now waits for this entire boundary;
// a retained worktree fails completion rather than pretending cleanup worked.
func (m *RunManager) completionCleanup(ag agent.Agent, gateDir, workDir, runID string, supervisor *shellenv.RunSupervisor) func() error {
	return sync.OnceValue(func() error {
		_ = ag.Close()
		// A cancelled run context must not prevent cleanup, but Git/filesystem
		// trouble must not leave completion waiting indefinitely either.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return cleanupRunWorktreeWithSupervisor(ctx, m.db, gateDir, workDir, runID, supervisor)
	})
}
