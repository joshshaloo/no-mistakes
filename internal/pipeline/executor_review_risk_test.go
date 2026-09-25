package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_ReadOnlyTailDoesNotRecheckRiskAfterStepReturn(t *testing.T) {
	database, p, run, repo := setupTest(t)
	review := &mockStep{name: types.StepReview, outcome: &StepOutcome{
		Findings: `{"findings":[],"risk_level":"low"}`, ReviewApprovedHeadSHA: run.HeadSHA,
	}}
	steps := []Step{review}
	for _, name := range []types.StepName{types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI} {
		steps = append(steps, &mockStep{name: name, outcome: &StepOutcome{Skipped: true}})
	}
	// No later step opens the worktree or runs an agent/command. A missing
	// worktree makes any accidental blanket freshness check observable: it
	// would turn LOW into STALE despite no work having been performed.
	exec := NewExecutor(database, p, &config.Config{}, nil, steps, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*results[0].FindingsJSON, `"risk_level":"low"`) {
		t.Fatalf("read-only tail rechecked the worktree: %s", *results[0].FindingsJSON)
	}
}

func TestExecutor_AgentRiskCheckFinishesBeforeStepCanPublishOutcome(t *testing.T) {
	database, p, run, repo := setupTest(t)
	review := &mockStep{name: types.StepReview, outcome: &StepOutcome{
		Findings: `{"findings":[],"risk_level":"low"}`, ReviewApprovedHeadSHA: run.HeadSHA,
	}}
	later := &adaptiveCallStep{name: types.StepDocument, fn: func(sctx *StepContext) (*StepOutcome, error) {
		_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{CWD: sctx.WorkDir})
		if err != nil {
			return nil, err
		}
		results, err := database.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		// A missing tree cannot establish freshness. The agent boundary must have
		// persisted that fact before handing control back to its caller, not at
		// step completion, in a background goroutine, or during cleanup.
		if !strings.Contains(*results[0].FindingsJSON, `"risk_level":"stale"`) {
			t.Errorf("agent returned before freshness was resolved: %s", *results[0].FindingsJSON)
		}
		return &StepOutcome{}, nil
	}}
	events := &eventCollector{}
	exec := NewExecutor(database, p, &config.Config{}, &promptCaptureAgent{}, []Step{review, later}, events.handler)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var invalidationEvent *ipc.Event
	for _, event := range events.all() {
		if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepReview && event.Findings != nil && strings.Contains(*event.Findings, `"risk_level":"stale"`) {
			copy := event
			invalidationEvent = &copy
		}
	}
	if invalidationEvent == nil {
		t.Fatal("review invalidation was not published to live subscribers")
	}
}
