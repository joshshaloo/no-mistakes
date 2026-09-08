package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPipelinePostTestDocumentFixFailureFailsBeforePush(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			if callCount == 1 {
				return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"docs need a copy fix","action":"auto-fix"}],"summary":"needs copy fix"}`)}, nil
			}
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("broken by docs\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"adjust copy"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{
		Test: "grep -q '^feature code$' feature.txt",
	})
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	exec := pipeline.NewExecutor(sctx.DB, p, sctx.Config, ag, []pipeline.Step{&TestStep{}, &DocumentStep{}, &PushStep{}}, nil)

	errCh := make(chan error, 1)
	go func() {
		errCh <- exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir)
	}()
	waitForStepStatusInStepsTest(t, sctx, types.StepDocument, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepDocument, types.ActionFix, nil); err != nil {
		t.Fatal(err)
	}
	err := <-errCh
	if err == nil {
		t.Fatal("expected run to fail before push")
	}
	if !strings.Contains(err.Error(), "final head verification failed") {
		t.Fatalf("error = %v, want final-head verification failure", err)
	}
	if callCount != 2 {
		t.Fatalf("document agent calls = %d, want initial plus fix round", callCount)
	}

	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var testFindings string
	for _, step := range steps {
		if step.StepName == types.StepTest && step.FindingsJSON != nil {
			testFindings = *step.FindingsJSON
		}
		if step.StepName == types.StepPush && step.Status != types.StepStatusFailed {
			t.Fatalf("push step status = %s, want failed before network push", step.Status)
		}
	}
	if testFindings == "" {
		t.Fatal("expected final-head verification findings on test step")
	}
	var findings types.Findings
	if err := json.Unmarshal([]byte(testFindings), &findings); err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings.Tested) != 1 || findings.Tested[0] != "grep -q '^feature code$' feature.txt" {
		t.Fatalf("tested = %#v, want configured test command", findings.Tested)
	}
}

func TestFinalHeadVerification_PostTestDocumentFixFailureRecordsAndFailsBeforePush(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{
		Test: "grep -q '^feature code$' feature.txt",
	})
	sctx.Shared = &pipeline.RunShared{}
	testResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteStep(testResult.ID, 0, 1, "test.log"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("broken by docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Fixing = true
	if err := commitAgentFixes(sctx, types.StepDocument, "adjust copy", "adjust copy"); err != nil {
		t.Fatal(err)
	}

	err = verifyFinalHeadAfterPostTestFixes(sctx)
	if err == nil {
		t.Fatal("expected final-head verification to fail")
	}
	if !strings.Contains(err.Error(), "final head verification failed") {
		t.Fatalf("error = %v, want final-head verification failure", err)
	}

	updated, err := sctx.DB.GetStepResult(testResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.FindingsJSON == nil {
		t.Fatal("expected final-head verification findings on test step")
	}
	var findings types.Findings
	if err := json.Unmarshal([]byte(*updated.FindingsJSON), &findings); err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if !strings.Contains(findings.TestingSummary, shortObjectID(sctx.Run.HeadSHA)) {
		t.Fatalf("testing summary %q does not identify final head %s", findings.TestingSummary, sctx.Run.HeadSHA)
	}
	if len(findings.Tested) != 1 || findings.Tested[0] != "grep -q '^feature code$' feature.txt" {
		t.Fatalf("tested = %#v, want configured test command", findings.Tested)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAutoFix {
		t.Fatalf("findings = %#v, want one auto-fix failure", findings.Items)
	}
	rounds, err := sctx.DB.GetRoundsByStep(testResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 || rounds[0].Trigger != postTestVerificationTrigger {
		t.Fatalf("test rounds = %#v, want final-head verification round", rounds)
	}
}

func waitForStepStatusInStepsTest(t *testing.T, sctx *pipeline.StepContext, stepName types.StepName, expected types.StepStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
		if err == nil {
			for _, step := range steps {
				if step.StepName == stepName && step.Status == expected {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("step %s did not reach status %q", stepName, expected)
}

func TestFinalHeadVerification_PostTestFixNoChangesDoesNotRerun(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	rerunLog := filepath.Join(dir, "rerun.log")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{
		Test: "printf rerun >> rerun.log",
	})
	sctx.Shared = &pipeline.RunShared{}
	sctx.Fixing = true
	if err := commitAgentFixes(sctx, types.StepDocument, "no changes", "no changes"); err != nil {
		t.Fatal(err)
	}
	if err := verifyFinalHeadAfterPostTestFixes(sctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rerunLog); !os.IsNotExist(err) {
		t.Fatalf("expected no verification rerun log, stat err = %v", err)
	}
}
