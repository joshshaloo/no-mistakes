package pipeline

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A blocked cleanup is a deterministic scheduling gap: neither the durable
// status nor the completion event may get ahead of worktree removal.
func TestExecutor_CancellationDuringCompletionCleanup(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		name := "cleanup-succeeds"
		if cleanupFails {
			name = "cleanup-fails"
		}
		t.Run(name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			exec := NewExecutor(database, p, nil, nil, nil, nil)
			cleanupReached := make(chan struct{})
			releaseCleanup := make(chan struct{})
			cleanupErr := errors.New("cleanup refused")
			exec.SetBeforeComplete(func() error {
				close(cleanupReached)
				<-releaseCleanup
				if cleanupFails {
					return cleanupErr
				}
				return nil
			})
			var terminalEvents []ipc.Event
			exec.onEvent = func(event ipc.Event) {
				if event.Type == ipc.EventRunCompleted {
					terminalEvents = append(terminalEvents, event)
				}
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- exec.Execute(ctx, run, repo, t.TempDir())
			}()
			select {
			case <-cleanupReached:
			case <-time.After(10 * time.Second):
				t.Fatal("cleanup barrier was never reached")
			}
			cause := errors.New(types.RunCancelReasonAbortedByUser)
			cancel(cause)
			close(releaseCleanup)
			var runErr error
			select {
			case runErr = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("executor did not finish after cleanup")
			}
			if runErr == nil || !strings.Contains(runErr.Error(), cause.Error()) {
				t.Fatalf("Execute error = %v, want cancellation cause", runErr)
			}
			if cleanupFails && !strings.Contains(runErr.Error(), cleanupErr.Error()) {
				t.Errorf("Execute error = %v, want cleanup diagnostic", runErr)
			}
			persisted, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != types.RunCancelled {
				t.Errorf("final status = %s, want %s", persisted.Status, types.RunCancelled)
			}
			if persisted.Error == nil || !strings.Contains(*persisted.Error, cause.Error()) {
				t.Errorf("durable error = %v, want cancellation cause", persisted.Error)
			}
			if cleanupFails && (persisted.Error == nil || !strings.Contains(*persisted.Error, cleanupErr.Error())) {
				t.Errorf("durable error = %v, want cleanup diagnostic", persisted.Error)
			}
			if len(terminalEvents) != 1 {
				t.Fatalf("terminal event count = %d, want 1", len(terminalEvents))
			}
			event := terminalEvents[0]
			if event.Status == nil || *event.Status != string(types.RunCancelled) {
				t.Errorf("terminal event = %+v, want cancelled", event)
			}
			if event.Error == nil || !strings.Contains(*event.Error, cause.Error()) {
				t.Errorf("terminal event error = %v, want cancellation cause", event.Error)
			}
			if cleanupFails && (event.Error == nil || !strings.Contains(*event.Error, cleanupErr.Error())) {
				t.Errorf("terminal event error = %v, want cleanup diagnostic", event.Error)
			}
		})
	}
}

func TestExecutor_CompletionWaitsForCleanup(t *testing.T) {
	for _, mode := range []string{"execute", "execute-skip", "resume", "resume-skip", "terminal-pr", "cleanup-error"} {
		t.Run(mode, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := t.TempDir()
			step := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
				if mode == "terminal-pr" {
					if err := sctx.UpdateRunPRState("merged"); err != nil {
						return nil, err
					}
				}
				return &StepOutcome{SkipRemaining: mode == "execute-skip"}, nil
			}}
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			if mode == "resume" || mode == "resume-skip" {
				if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				sr, err := database.InsertStepResult(run.ID, types.StepCI)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.StartStep(sr.ID); err != nil {
					t.Fatal(err)
				}
				findings := `{"findings":[{"id":"ci-1","severity":"warning","description":"decision","action":"ask-user"}]}`
				if err := database.SetStepFindings(sr.ID, findings); err != nil {
					t.Fatal(err)
				}
				if _, err := database.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 1); err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateStepStatusWithDuration(sr.ID, types.StepStatusAwaitingApproval, 1); err != nil {
					t.Fatal(err)
				}
				if err := database.SetRunAwaitingAgent(run.ID); err != nil {
					t.Fatal(err)
				}
				run, err = database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			cleanupReached := make(chan struct{})
			releaseCleanup := make(chan struct{})
			cleanupErr := errors.New("cleanup refused")
			exec.SetBeforeComplete(func() error {
				close(cleanupReached)
				<-releaseCleanup
				if mode == "cleanup-error" {
					return cleanupErr
				}
				return os.Remove(workDir)
			})
			terminal := make(chan ipc.Event, 1)
			exec.onEvent = func(event ipc.Event) {
				if event.Type == ipc.EventRunCompleted {
					terminal <- event
					if mode != "cleanup-error" {
						if _, err := os.Stat(workDir); !os.IsNotExist(err) {
							t.Errorf("completion event precedes worktree removal: %v", err)
						}
					}
				}
				if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
					action := types.ActionApprove
					if mode == "resume-skip" {
						action = types.ActionSkip
					}
					if err := exec.Respond(types.StepCI, action, nil); err != nil {
						t.Error(err)
					}
				}
			}
			done := make(chan error, 1)
			go func() {
				if mode == "resume" || mode == "resume-skip" {
					done <- exec.Resume(context.Background(), run, repo, workDir)
				} else {
					done <- exec.Execute(context.Background(), run, repo, workDir)
				}
			}()
			select {
			case <-cleanupReached:
			case err := <-done:
				t.Fatalf("executor exited without cleanup: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("cleanup barrier was never reached")
			}
			persisted, err := database.GetRun(run.ID)
			if err != nil {
				t.Error(err)
			} else if persisted.Status != types.RunRunning {
				t.Errorf("status during cleanup = %s, want running", persisted.Status)
			}
			select {
			case event := <-terminal:
				t.Errorf("premature completion event: %+v", event)
			default:
			}
			close(releaseCleanup)
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("executor did not finish after cleanup")
			}
			if mode == "cleanup-error" {
				if !errors.Is(err, cleanupErr) {
					t.Errorf("Execute error = %v, want cleanup error", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			persisted, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := types.RunCompleted
			if mode == "cleanup-error" {
				want = types.RunFailed
			}
			if persisted.Status != want {
				t.Errorf("final status = %s, want %s", persisted.Status, want)
			}
			select {
			case event := <-terminal:
				if event.Status == nil || *event.Status != string(want) {
					t.Errorf("completion event = %+v, want status %s", event, want)
				}
			default:
				t.Error("missing completion event after cleanup")
			}
		})
	}
}
