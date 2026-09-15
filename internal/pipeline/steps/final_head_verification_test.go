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
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
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
	testResult := completeTestStepWithVerifiedTree(t, sctx, "")

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("broken by docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Fixing = true
	if err := commitAgentFixes(sctx, types.StepDocument, "adjust copy", "adjust copy"); err != nil {
		t.Fatal(err)
	}

	err := verifyFinalHeadAfterPostTestFixes(sctx)
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
	completeTestStepWithVerifiedTree(t, sctx, "")
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

// completeTestStepWithVerifiedTree records the shape the executor produces for
// a Test step that completed with green evidence: stored findings plus the
// durable test-verified tree anchor, written in one transaction. The anchor is
// the working tree as it stood when the tests ran, uncommitted agent files
// included.
func completeTestStepWithVerifiedTree(t *testing.T, sctx *pipeline.StepContext, findingsJSON string) *db.StepResult {
	t.Helper()
	verifiedTree, err := git.WorktreeTreeSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	testResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if findingsJSON != "" {
		if err := sctx.DB.SetStepFindings(testResult.ID, findingsJSON); err != nil {
			t.Fatal(err)
		}
	}
	if err := sctx.DB.CompleteTestStep(testResult.ID, sctx.Run.ID, verifiedTree, 0, 1, "test.log"); err != nil {
		t.Fatal(err)
	}
	return testResult
}

func captureStepLog(sctx *pipeline.StepContext) *[]string {
	var lines []string
	sctx.Log = func(line string) { lines = append(lines, line) }
	return &lines
}

func loadTestStepFindings(t *testing.T, sctx *pipeline.StepContext, stepResultID string) types.Findings {
	t.Helper()
	updated, err := sctx.DB.GetStepResult(stepResultID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.FindingsJSON == nil {
		t.Fatal("test step has no recorded findings")
	}
	findings, err := types.ParseFindingsJSON(*updated.FindingsJSON)
	if err != nil {
		t.Fatalf("parse merged findings: %v", err)
	}
	return findings
}

// A post-test document fix must never cost the Test step's own evidence: the
// PR body renders artifacts, tested entries, and the testing summary from the
// step's stored findings, so the verification is merged into them.
func TestFinalHeadVerification_PreservesTestEvidenceAfterDocumentFix(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sctx.Shared = &pipeline.RunShared{}
	original := types.Findings{
		Items:          []types.Finding{{Severity: "info", Action: types.ActionNoOp, Description: "new test file written by agent: feature_test.go"}},
		Summary:        "tests passed with new coverage",
		Tested:         []string{"`go test ./internal/feature`"},
		TestingSummary: "Exercised the feature end to end and captured a screenshot.",
		Artifacts:      []types.TestArtifact{{Kind: "image", Label: "feature screenshot", Path: "evidence/feature.png"}},
	}
	originalJSON, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	testResult := completeTestStepWithVerifiedTree(t, sctx, string(originalJSON))

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("documented\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Fixing = true
	if err := commitAgentFixes(sctx, types.StepDocument, "document feature", "document feature"); err != nil {
		t.Fatal(err)
	}
	if err := verifyFinalHeadAfterPostTestFixes(sctx); err != nil {
		t.Fatalf("verification failed: %v", err)
	}

	merged := loadTestStepFindings(t, sctx, testResult.ID)
	if len(merged.Artifacts) != 1 || merged.Artifacts[0].Path != "evidence/feature.png" {
		t.Fatalf("artifacts = %#v, want the original evidence artifact preserved", merged.Artifacts)
	}
	if len(merged.Tested) == 0 || merged.Tested[0] != "`go test ./internal/feature`" {
		t.Fatalf("tested = %#v, want the original tested entry preserved", merged.Tested)
	}
	if !containsString(merged.Tested, "true") {
		t.Fatalf("tested = %#v, want the verification command appended", merged.Tested)
	}
	if !strings.Contains(merged.TestingSummary, "captured a screenshot") {
		t.Fatalf("testing summary %q lost the original evidence", merged.TestingSummary)
	}
	if !strings.Contains(merged.TestingSummary, shortObjectID(sctx.Run.HeadSHA)) {
		t.Fatalf("testing summary %q does not add the final-head verification", merged.TestingSummary)
	}
	// The informational finding must survive, or the PR renders the Test step
	// as "fixed" when nothing was ever fixed.
	if len(merged.Items) != 1 || merged.Items[0].Action != types.ActionNoOp {
		t.Fatalf("items = %#v, want the original informational finding retained", merged.Items)
	}
}

// The invariant must survive a daemon restart: a run resumed after a parked
// document commit has an empty in-memory marker, so only the durable anchor
// can still trigger re-verification.
func TestFinalHeadVerification_ResumedRunWithoutMarkerStillReverifies(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{
		Test: "grep -q '^feature code$' feature.txt",
	})
	sctx.Shared = &pipeline.RunShared{}
	testResult := completeTestStepWithVerifiedTree(t, sctx, `{"findings":[],"summary":"tests passed"}`)

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("broken by docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Fixing = true
	if err := commitAgentFixes(sctx, types.StepDocument, "adjust copy", "adjust copy"); err != nil {
		t.Fatal(err)
	}

	// Resume rebuilds run scopes, so the marker the document step set is gone.
	sctx.Shared = &pipeline.RunShared{}
	if _, marked := sctx.Shared.PendingPostTestFixChange(); marked {
		t.Fatal("resumed run unexpectedly retained the in-memory marker")
	}

	err := verifyFinalHeadAfterPostTestFixes(sctx)
	if err == nil {
		t.Fatal("resumed run pushed a head the test step never validated")
	}
	if !strings.Contains(err.Error(), "final head verification failed") {
		t.Fatalf("error = %v, want final-head verification failure", err)
	}
	merged := loadTestStepFindings(t, sctx, testResult.ID)
	if len(merged.Items) != 1 || merged.Items[0].Severity != "error" {
		t.Fatalf("items = %#v, want the verification failure recorded", merged.Items)
	}
}

// A Test step that never produced green evidence has nothing to invalidate:
// the boundary records why and no-ops instead of running tests the user
// opted out of and blocking the push on them.
func TestFinalHeadVerification_SkippedTestStepNoOpsWithReason(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	rerunLog := filepath.Join(dir, "rerun.log")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{
		Test: "printf rerun >> rerun.log; exit 1",
	})
	sctx.Shared = &pipeline.RunShared{}
	testResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteStepWithStatus(testResult.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	logged := captureStepLog(sctx)

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("changed by docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Fixing = true
	if err := commitAgentFixes(sctx, types.StepDocument, "adjust copy", "adjust copy"); err != nil {
		t.Fatal(err)
	}
	if err := verifyFinalHeadAfterPostTestFixes(sctx); err != nil {
		t.Fatalf("skipped test step blocked the push: %v", err)
	}
	if _, err := os.Stat(rerunLog); !os.IsNotExist(err) {
		t.Fatalf("verification ran tests against a skipped test step, stat err = %v", err)
	}
	updated, err := sctx.DB.GetStepResult(testResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.FindingsJSON != nil {
		t.Fatalf("skipped test step gained manufactured evidence: %s", *updated.FindingsJSON)
	}
	if !containsSubstring(*logged, "skipping final head verification: the test step is skipped") {
		t.Fatalf("log = %#v, want the concrete skip reason recorded", *logged)
	}
}

// The push step's own format-and-commit path is the third post-Test commit
// point; a formatter rewrite alone must still re-verify the final head.
func TestPushStep_FormatOnlyChangeReverifiesFinalHead(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{
		Format: "printf 'reformatted\n' > feature.txt",
		Test:   "grep -q '^feature code$' feature.txt",
	})
	sctx.Shared = &pipeline.RunShared{}
	recordReviewApproval(t, sctx, headSHA)
	completeTestStepWithVerifiedTree(t, sctx, `{"findings":[],"summary":"tests passed"}`)

	outcome, err := (&PushStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("push shipped a formatter rewrite the test step never validated")
	}
	if outcome != nil {
		t.Fatalf("outcome = %#v, want nil on step failure", outcome)
	}
	if !strings.Contains(err.Error(), "final head verification failed") {
		t.Fatalf("error = %v, want final-head verification failure", err)
	}
}

// commands.format is a configured tool like commands.test and commands.lint:
// an absent binary fails the owning step with a concrete message instead of
// being logged as a warning and treated as success.
func TestPushStep_MissingConfiguredFormatterFailsStep(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{
		Format: "no-mistakes-missing-formatter",
	})
	sctx.Shared = &pipeline.RunShared{}

	outcome, err := (&PushStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected missing configured formatter to fail the push step")
	}
	if outcome != nil {
		t.Fatalf("outcome = %#v, want nil on step failure", outcome)
	}
	if !strings.Contains(err.Error(), "configured format command could not run") || !strings.Contains(err.Error(), "exit code 127") {
		t.Fatalf("error = %v, want concrete command-not-found failure", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

// The test agent routinely writes focused tests that no step commits until the
// push stage. That content was validated by the tests that just ran, so
// landing it must not trigger a second full verification pass.
func TestFinalHeadVerification_TestAgentFilesCommittedByPushDoNotReverify(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	rerunLog := filepath.Join(dir, "rerun.log")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{
		Test: "printf rerun >> rerun.log",
	})
	sctx.Shared = &pipeline.RunShared{}

	// The test agent writes a new focused test and leaves it uncommitted.
	if err := os.WriteFile(filepath.Join(dir, "feature_extra_test.go"), []byte("package feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	completeTestStepWithVerifiedTree(t, sctx, `{"findings":[],"summary":"tests passed"}`)
	recordReviewApproval(t, sctx, headSHA)

	outcome, err := (&PushStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected the push step to reach the network push")
	}
	if outcome != nil {
		t.Fatalf("outcome = %#v, want nil on step failure", outcome)
	}
	if strings.Contains(err.Error(), "final head verification") {
		t.Fatalf("re-verified content the test step already validated: %v", err)
	}
	if _, err := os.Stat(rerunLog); !os.IsNotExist(err) {
		t.Fatalf("verification re-ran tests for the test step's own files, stat err = %v", err)
	}
	if lastCommitMessage(t, dir) != "no-mistakes: apply agent fixes" {
		t.Fatalf("push step did not commit the test agent's file: %s", lastCommitMessage(t, dir))
	}
}

// In-repo evidence artifacts exist in the working tree while the tests run and
// are only staged at the push boundary, so committing them changes nothing the
// tests validated.
func TestFinalHeadVerification_InRepoEvidenceStagingDoesNotReverify(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	rerunLog := filepath.Join(dir, "rerun.log")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{
		Test: "printf rerun >> rerun.log",
	})
	sctx.Config.Test.Evidence.StoreInRepo = true
	sctx.Config.Test.Evidence.Dir = "docs/evidence"
	sctx.Shared = &pipeline.RunShared{}

	location := resolveTestEvidenceLocation(dir, sctx.Run.Branch, sctx.Run.ID, sctx.Config.Test.Evidence)
	if !location.StoreInRepo {
		t.Fatalf("evidence location %s is not in the repository", location.Dir)
	}
	evidenceDir := location.Dir
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "screenshot.txt"), []byte("evidence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	completeTestStepWithVerifiedTree(t, sctx, `{"findings":[],"summary":"tests passed"}`)
	recordReviewApproval(t, sctx, headSHA)

	outcome, err := (&PushStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected the push step to reach the network push")
	}
	if outcome != nil {
		t.Fatalf("outcome = %#v, want nil on step failure", outcome)
	}
	if strings.Contains(err.Error(), "final head verification") {
		t.Fatalf("re-verified pipeline-staged evidence artifacts: %v", err)
	}
	if _, err := os.Stat(rerunLog); !os.IsNotExist(err) {
		t.Fatalf("verification re-ran tests for staged evidence, stat err = %v", err)
	}
	rel, err := filepath.Rel(dir, filepath.Join(evidenceDir, "screenshot.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if tracked := gitCmd(t, dir, "ls-tree", "--name-only", "HEAD", filepath.ToSlash(rel)); tracked == "" {
		t.Fatalf("evidence artifact %s was never committed, so the test proves nothing", rel)
	}
}

// A document fix to source is a genuine change to validated content and must
// still re-verify even though the tree anchor tolerates the pipeline's own
// staging.
func TestFinalHeadVerification_DocumentFixToSourceStillReverifies(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{
		Test: "grep -q '^feature code$' feature.txt",
	})
	sctx.Shared = &pipeline.RunShared{}
	if err := os.WriteFile(filepath.Join(dir, "feature_extra_test.go"), []byte("package feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	completeTestStepWithVerifiedTree(t, sctx, `{"findings":[],"summary":"tests passed"}`)

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("broken by docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Fixing = true
	if err := commitAgentFixes(sctx, types.StepDocument, "adjust copy", "adjust copy"); err != nil {
		t.Fatal(err)
	}
	err := verifyFinalHeadAfterPostTestFixes(sctx)
	if err == nil {
		t.Fatal("a post-test source change was shipped without re-verification")
	}
	if !strings.Contains(err.Error(), "final head verification failed") {
		t.Fatalf("error = %v, want final-head verification failure", err)
	}
}

// A verification that fails must never be recorded with success wording. A
// quiet failing command (a silent grep, a test runner that writes nothing on
// failure) produces no stdout, so the summary can only come from an explicit
// failure phrasing rather than from a default that assumes success.
func TestFinalHeadVerification_SilentFailingCommandRecordsFailureWording(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{
		Test: "exit 3",
	})
	sctx.Shared = &pipeline.RunShared{}
	testResult := completeTestStepWithVerifiedTree(t, sctx, "")

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("documented\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Fixing = true
	if err := commitAgentFixes(sctx, types.StepDocument, "document feature", "document feature"); err != nil {
		t.Fatal(err)
	}

	if err := verifyFinalHeadAfterPostTestFixes(sctx); err == nil {
		t.Fatal("expected final-head verification to fail on a non-zero exit")
	}

	merged := loadTestStepFindings(t, sctx, testResult.ID)
	headLabel := shortObjectID(sctx.Run.HeadSHA)
	for name, text := range map[string]string{"summary": merged.Summary, "testing_summary": merged.TestingSummary} {
		if strings.Contains(text, "verified") {
			t.Fatalf("%s = %q, want failure wording for a failed verification", name, text)
		}
		if !strings.Contains(text, headLabel) {
			t.Fatalf("%s = %q, want the final head named", name, text)
		}
		if !strings.Contains(text, "exit 3") {
			t.Fatalf("%s = %q, want the exit code named", name, text)
		}
	}
}

// Summary sentences accumulate, so a later passing round must add its own
// sentence without erasing the recorded failure.
func TestFinalHeadVerification_LaterSuccessKeepsRecordedFailureSentence(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{
		Test: "grep -q '^feature code$' feature.txt",
	})
	sctx.Shared = &pipeline.RunShared{}
	testResult := completeTestStepWithVerifiedTree(t, sctx, "")

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("broken by docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Fixing = true
	if err := commitAgentFixes(sctx, types.StepDocument, "adjust copy", "adjust copy"); err != nil {
		t.Fatal(err)
	}
	if err := verifyFinalHeadAfterPostTestFixes(sctx); err == nil {
		t.Fatal("expected the first verification to fail")
	}
	failedHead := shortObjectID(sctx.Run.HeadSHA)

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature code\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Also touch an unrelated file so the restored tree differs from the
	// test-verified anchor and the boundary actually re-verifies.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("documented\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAgentFixes(sctx, types.StepDocument, "restore copy", "restore copy"); err != nil {
		t.Fatal(err)
	}
	if err := verifyFinalHeadAfterPostTestFixes(sctx); err != nil {
		t.Fatalf("second verification failed: %v", err)
	}

	merged := loadTestStepFindings(t, sctx, testResult.ID)
	if !strings.Contains(merged.TestingSummary, failedHead) {
		t.Fatalf("testing summary %q dropped the recorded failure for head %s", merged.TestingSummary, failedHead)
	}
	if !strings.Contains(merged.TestingSummary, "verified") {
		t.Fatalf("testing summary %q lost the passing round", merged.TestingSummary)
	}
}

// The merged testing summary is rendered into the PR body, where a newline
// routes the whole sentence through the escaped <code> span path.
func TestFinalHeadVerification_MergedTestingSummaryRendersAsProse(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sctx.Shared = &pipeline.RunShared{}
	original := types.Findings{
		Summary:        "tests passed with new coverage",
		Tested:         []string{"`go test ./internal/feature`"},
		TestingSummary: "Exercised the feature end to end and captured the CLI transcript.",
	}
	originalJSON, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	completeTestStepWithVerifiedTree(t, sctx, string(originalJSON))

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("documented\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Fixing = true
	if err := commitAgentFixes(sctx, types.StepDocument, "document feature", "document feature"); err != nil {
		t.Fatal(err)
	}
	if err := verifyFinalHeadAfterPostTestFixes(sctx); err != nil {
		t.Fatalf("verification failed: %v", err)
	}

	stepResults, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds := map[string][]*db.StepRound{}
	for _, step := range stepResults {
		stepRounds, err := sctx.DB.GetRoundsByStep(step.ID)
		if err != nil {
			t.Fatal(err)
		}
		rounds[step.ID] = stepRounds
	}
	md := BuildTestingSummary(stepResults, rounds)

	if strings.Contains(md, "<code>") || strings.Contains(md, "&#10;") {
		t.Fatalf("testing block renders the merged summary as an escaped code span:\n%s", md)
	}
	if !strings.Contains(md, "- Summary: Exercised the feature end to end and captured the CLI transcript.") {
		t.Fatalf("testing block lost the prose summary:\n%s", md)
	}
}
