package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type riskCleanupAgent struct{}

func (*riskCleanupAgent) Name() string { return "risk-cleanup-test" }
func (*riskCleanupAgent) Close() error { return nil }
func (*riskCleanupAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	err := os.WriteFile(filepath.Join(opts.CWD, "feature.go"), []byte("package feature\nconst Changed = true\n"), 0o644)
	return &agent.Result{}, err
}

// PR #10's completed-review update is not a completed-run event. It must remain
// live while PR #9's cleanup barrier delays both durable and IPC run completion,
// including when a CI step observes terminal PR truth in Execute or Resume.
func TestExecutor_ReviewRiskUpdatePrecedesCompletionCleanup(t *testing.T) {
	for _, mode := range []string{"execute", "resume"} {
		for _, cleanupResult := range []string{"success", "refusal"} {
			t.Run(mode+"/"+cleanupResult, func(t *testing.T) {
				database, p, run, repo := setupTest(t)
				workDir := t.TempDir()
				initGitRepo(t, workDir)
				writeTestFile(t, workDir, "feature.go", "package feature\nconst Changed = false\n")
				execGit(t, workDir, "add", "feature.go")
				execGit(t, workDir, "commit", "-m", "reviewed source")
				head, err := git.HeadSHA(context.Background(), workDir)
				if err != nil {
					t.Fatal(err)
				}
				run.HeadSHA = head
				if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
					t.Fatal(err)
				}
				low := `{"findings":[],"risk_level":"low","risk_rationale":"reviewed source"}`
				var reviewedRound string
				steps := []Step{
					&mockStep{name: types.StepReview, outcome: &StepOutcome{Findings: low, ReviewApprovedHeadSHA: head}},
					newPassStep(types.StepTest),
					&adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
						results, err := database.GetStepsByRun(run.ID)
						if err != nil {
							return nil, err
						}
						rounds, err := database.GetRoundsByStep(results[0].ID)
						if err != nil || len(rounds) != 1 || rounds[0].FindingsJSON == nil {
							return nil, errors.New("missing completed review evidence before mutation")
						}
						reviewedRound = *rounds[0].FindingsJSON
						findings, err := types.ParseFindingsJSON(reviewedRound)
						if err != nil || findings.RiskLevel != "low" {
							return nil, errors.New("review is not low risk before mutation")
						}
						if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{CWD: sctx.WorkDir}); err != nil {
							return nil, err
						}
						if err := sctx.UpdateRunPRState("merged"); err != nil {
							return nil, err
						}
						return &StepOutcome{}, nil
					}},
				}
				if mode == "resume" {
					if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
						t.Fatal(err)
					}
					for _, step := range steps {
						sr, err := database.InsertStepResult(run.ID, step.Name())
						if err != nil {
							t.Fatal(err)
						}
						if step.Name() == types.StepCI {
							continue
						}
						if err := database.StartStep(sr.ID); err != nil {
							t.Fatal(err)
						}
						findings := low
						if step.Name() == types.StepTest {
							findings = `{"findings":[{"id":"test-1","severity":"warning","description":"decision","action":"ask-user"}]}`
						}
						if err := database.SetStepFindings(sr.ID, findings); err != nil {
							t.Fatal(err)
						}
						if _, err := database.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 17); err != nil {
							t.Fatal(err)
						}
						if step.Name() == types.StepReview {
							err = database.CompleteReviewStep(sr.ID, run.ID, head, 0, 17, "")
						} else {
							err = database.UpdateStepStatusWithDuration(sr.ID, types.StepStatusAwaitingApproval, 17)
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					if err := database.SetRunAwaitingAgent(run.ID); err != nil {
						t.Fatal(err)
					}
					run, err = database.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
				}

				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				cleanupReached := make(chan struct{})
				releaseCleanup := make(chan struct{})
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(releaseCleanup) }) }
				cleanupErr := errors.New("interaction cleanup refused")
				executor := NewExecutor(database, p, &config.Config{}, &riskCleanupAgent{}, steps, nil)
				executor.SetBeforeComplete(func() error {
					close(cleanupReached)
					<-releaseCleanup
					if cleanupResult == "refusal" {
						return cleanupErr
					}
					return os.RemoveAll(workDir)
				})
				events := &eventCollector{}
				executor.onEvent = func(event ipc.Event) {
					events.handler(event)
					if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepTest &&
						event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
						if err := executor.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
							t.Error(err)
						}
					}
				}
				done := make(chan error, 1)
				finished := make(chan struct{})
				t.Cleanup(func() {
					cancel()
					release()
					select {
					case <-finished:
					case <-time.After(10 * time.Second):
						t.Error("executor did not stop after releasing cleanup")
					}
				})
				go func() {
					defer close(finished)
					if mode == "resume" {
						done <- executor.Resume(ctx, run, repo, workDir)
					} else {
						done <- executor.Execute(ctx, run, repo, workDir)
					}
				}()
				select {
				case <-cleanupReached:
				case err := <-done:
					t.Fatalf("execution ended without cleanup barrier: %v", err)
				case <-ctx.Done():
					t.Fatal("cleanup barrier was not reached")
				}

				persisted, err := database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				prState := ""
				if persisted.PRState != nil {
					prState = *persisted.PRState
				}
				if persisted.Status != types.RunRunning || prState != "merged" {
					t.Errorf("during cleanup: run=%s PR=%s, want running/merged", persisted.Status, prState)
				}
				if event := events.findRunEvent(ipc.EventRunCompleted); event != nil {
					t.Errorf("run completed before cleanup: %+v", event)
				}
				if _, err := os.Stat(workDir); err != nil {
					t.Fatalf("worktree absent before cleanup release: %v", err)
				}
				results, err := database.GetStepsByRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				review := results[0]
				if review.FindingsJSON == nil || !strings.Contains(*review.FindingsJSON, `"risk_level":"stale"`) {
					t.Fatalf("review risk not durably stale before cleanup: %+v", review)
				}
				var invalidation *ipc.Event
				for _, event := range events.all() {
					if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepReview &&
						event.Findings != nil && strings.Contains(*event.Findings, `"risk_level":"stale"`) {
						copy := event
						invalidation = &copy
					}
				}
				if invalidation == nil {
					t.Fatal("live review invalidation missing before cleanup")
				}
				if invalidation.DurationMS == nil || review.DurationMS == nil || *invalidation.DurationMS != *review.DurationMS {
					t.Fatal("live risk update changed the original review duration")
				}
				rounds, err := database.GetRoundsByStep(review.ID)
				if err != nil || len(rounds) != 1 || rounds[0].FindingsJSON == nil || *rounds[0].FindingsJSON != reviewedRound {
					t.Fatalf("historical review was changed: rounds=%+v err=%v", rounds, err)
				}

				release()
				select {
				case err = <-done:
				case <-ctx.Done():
					t.Fatal("execution did not finish after cleanup release")
				}
				wantStatus := types.RunCompleted
				if cleanupResult == "refusal" {
					wantStatus = types.RunFailed
					if !errors.Is(err, cleanupErr) {
						t.Fatalf("completion error = %v, want cleanup refusal", err)
					}
					if _, err := os.Stat(workDir); err != nil {
						t.Fatalf("cleanup refusal discarded worktree: %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(workDir); !os.IsNotExist(err) {
						t.Fatalf("completion preceded worktree removal: %v", err)
					}
				}
				persisted, err = database.GetRun(run.ID)
				if err != nil || persisted.Status != wantStatus {
					t.Fatalf("final run = %+v, err=%v; want %s", persisted, err, wantStatus)
				}
				var terminalCount int
				for _, event := range events.all() {
					if event.Type == ipc.EventRunCompleted {
						terminalCount++
						if event.Status == nil || *event.Status != string(wantStatus) {
							t.Errorf("terminal status = %v, want %s", event.Status, wantStatus)
						}
					}
				}
				if terminalCount != 1 {
					t.Fatalf("terminal event count = %d, want one", terminalCount)
				}
				t.Logf("risk=stale, history=low, live update before cleanup; %s terminal=%s after cleanup", mode, wantStatus)
			})
		}
	}
}
