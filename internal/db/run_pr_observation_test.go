package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestObserveRunPRStateLeavesCompletionToOwner(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			d := openTestDB(t)
			repo, err := d.InsertRepo("/test/observed-pr-"+state, "git@github.com:user/project.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			ci, err := d.InsertStepResult(run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.StartStep(ci.ID); err != nil {
				t.Fatal(err)
			}
			for _, observed := range []string{state, "open", state} {
				if err := d.ObserveRunPRState(run.ID, observed); err != nil {
					t.Fatal(err)
				}
				got, err := d.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Status != types.RunRunning || got.PRState == nil || *got.PRState != state {
					t.Fatalf("observed run = %+v; want running with monotonic PR state %s", got, state)
				}
				step, err := d.GetStepResult(ci.ID)
				if err != nil {
					t.Fatal(err)
				}
				if step.Status != types.StepStatusRunning {
					t.Fatalf("observation prematurely completed CI: %s", step.Status)
				}
			}
			candidates, err := d.TerminalPRCompletionCandidates()
			if err != nil || len(candidates) != 1 {
				t.Fatalf("completion candidates = %d, %v", len(candidates), err)
			}
			if err := d.CompleteSuccessfulRun(run.ID, true); err != nil {
				t.Fatal(err)
			}
			got, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != types.RunCompleted {
				t.Fatalf("reconciled status = %s", got.Status)
			}
		})
	}
}
