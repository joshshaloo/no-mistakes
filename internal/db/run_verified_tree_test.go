package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCompleteTestStepRecordsVerifiedTreeAtomically(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "mutable", "base")
	if err != nil {
		t.Fatal(err)
	}
	if run.TestVerifiedTreeSHA != nil {
		t.Fatal("new run inferred a verified tree")
	}
	step, err := d.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteTestStep(step.ID, run.ID, "verified-1", 0, 5, "test.log"); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TestVerifiedTreeSHA == nil || *got.TestVerifiedTreeSHA != "verified-1" {
		t.Fatalf("verified tree = %v", got.TestVerifiedTreeSHA)
	}
	gotStep, err := d.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotStep.Status != types.StepStatusCompleted {
		t.Fatalf("step status = %s", gotStep.Status)
	}
	if err := d.CompleteTestStep("missing-step", run.ID, "verified-2", 0, 5, "test.log"); err == nil {
		t.Fatal("unknown step completion must fail")
	}
	got, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TestVerifiedTreeSHA == nil || *got.TestVerifiedTreeSHA != "verified-1" {
		t.Fatal("failed transaction changed the anchor")
	}
	if err := d.UpdateRunTestVerifiedTreeSHA(run.ID, "verified-3"); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TestVerifiedTreeSHA == nil || *got.TestVerifiedTreeSHA != "verified-3" {
		t.Fatal("reverification did not change the anchor")
	}
}
