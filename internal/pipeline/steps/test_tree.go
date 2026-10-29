package steps

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// A green round binds the exact worktree content, including tests the agent
// wrote that a later step will commit. Parked rounds cannot confer evidence.
func withTestVerifiedTree(sctx *pipeline.StepContext, outcome *pipeline.StepOutcome) (*pipeline.StepOutcome, error) {
	if outcome.NeedsApproval {
		return outcome, nil
	}
	tree, err := git.WorktreeTreeSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve tested working tree: %w", err)
	}
	outcome.TestVerifiedTreeSHA = tree
	return outcome, nil
}
