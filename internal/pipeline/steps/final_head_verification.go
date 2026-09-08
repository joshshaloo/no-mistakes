package steps

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const postTestVerificationTrigger = "post_test_fix_verification"

// verifyFinalHeadAfterPostTestFixes is the single owner for the rule that a
// pipeline-owned commit made by a later local step invalidates the earlier Test
// result. The push boundary calls it after all local mutation/commit points and
// before network push, so the run's recorded test evidence describes the exact
// head being shipped.
func verifyFinalHeadAfterPostTestFixes(sctx *pipeline.StepContext) error {
	if sctx.Shared == nil {
		return nil
	}
	change, ok := sctx.Shared.PendingPostTestFixChange()
	if !ok {
		return nil
	}
	currentHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve final head for post-test verification: %w", err)
	}
	changedFiles, err := postTestChangedFiles(sctx, change.FromHead, currentHead)
	if err != nil {
		return err
	}
	if len(changedFiles) == 0 {
		sctx.Shared.CompletePostTestFixVerification()
		return nil
	}

	testStepResultID, err := testStepResultIDForRun(sctx)
	if err != nil {
		return err
	}
	started := time.Now()
	steps := joinStepNames(change.Steps)
	headLabel := shortObjectID(currentHead)

	testCmd := ""
	if sctx.Config != nil {
		testCmd = strings.TrimSpace(sctx.Config.Commands.Test)
	}
	if testCmd != "" {
		sctx.Log(fmt.Sprintf("re-running test verification on final head %s after post-test %s changes: %s", headLabel, steps, testCmd))
		output, exitCode, err := runConfiguredStepShellCommand(sctx, types.StepTest, testCmd)
		tested := []string{testCmd}
		projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepTest)
		if err != nil {
			findings := finalHeadVerificationFindings(currentHead, changedFiles, tested, err.Error(), []Finding{{
				Severity:    "error",
				Action:      types.ActionAutoFix,
				Description: err.Error(),
			}})
			if recordErr := recordFinalHeadVerificationRound(sctx, testStepResultID, findings, time.Since(started).Milliseconds()); recordErr != nil {
				return fmt.Errorf("record final head verification failure: %w (original failure: %v)", recordErr, err)
			}
			return fmt.Errorf("final head verification failed after post-test fixes: %w", err)
		}
		if exitCode != 0 {
			description := fmt.Sprintf("tests failed on final head after post-test fixes (exit code %d)", exitCode)
			findings := finalHeadVerificationFindings(currentHead, changedFiles, tested, projectedOutput, []Finding{{
				Severity:    "error",
				Action:      types.ActionAutoFix,
				Description: description,
			}})
			if recordErr := recordFinalHeadVerificationRound(sctx, testStepResultID, findings, time.Since(started).Milliseconds()); recordErr != nil {
				return fmt.Errorf("record final head verification failure: %w (original failure: %s)", recordErr, description)
			}
			return fmt.Errorf("final head verification failed after post-test fixes: %s", description)
		}
		findings := finalHeadVerificationFindings(currentHead, changedFiles, tested, "", nil)
		if err := recordFinalHeadVerificationRound(sctx, testStepResultID, findings, time.Since(started).Milliseconds()); err != nil {
			return err
		}
		sctx.Shared.CompletePostTestFixVerification()
		return nil
	}

	if sctx.Agent == nil {
		return fmt.Errorf("final head verification required after post-test fixes, but no test command or agent is available")
	}
	sctx.Log(fmt.Sprintf("asking agent to re-run affected test verification on final head %s after post-test %s changes...", headLabel, steps))
	result, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
		Prompt: fmt.Sprintf(`Re-verify the final head after pipeline-owned document/lint changes landed after the Test step.

Context:
- branch: %s
- final head: %s
- changed after Test: %s

Task:
- Run the workspace typecheck if this repository configures one.
- Run the smallest test suites or selectors that cover the changed files listed above.
- Do not run linters, formatters, or broad unrelated regression suites.
- Do not edit source, tests, docs, or committed artifacts; this is verification only.
- Return the same JSON shape as the Test step, including a non-empty tested array and any unresolved failures as findings.`,
			sctx.Run.Branch,
			currentHead,
			strings.Join(changedFiles, ", "),
		),
		CWD:        sctx.WorkDir,
		JSONSchema: testFindingsSchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    "test-final-head",
	})
	if err != nil {
		return fmt.Errorf("final head verification agent: %w", err)
	}
	var findings Findings
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &findings); err != nil {
			sctx.Log("could not parse final-head verification structured output, using text response")
			findings = Findings{Summary: result.Text}
		}
	}
	findings = finalHeadVerificationFindings(currentHead, changedFiles, findings.Tested, findings.Summary, findings.Items)
	if len(findings.Tested) == 0 {
		findings.Items = append(findings.Items, Finding{
			Severity:    "error",
			Action:      types.ActionAutoFix,
			Description: "final head verification did not report any test, typecheck, or manual verification command",
		})
	}
	if err := recordFinalHeadVerificationRound(sctx, testStepResultID, findings, time.Since(started).Milliseconds()); err != nil {
		return err
	}
	if hasBlockingFindings(findings.Items) {
		return fmt.Errorf("final head verification failed after post-test fixes: %s", findings.Summary)
	}
	sctx.Shared.CompletePostTestFixVerification()
	return nil
}

func postTestChangedFiles(sctx *pipeline.StepContext, fromHead, toHead string) ([]string, error) {
	out, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-only", strings.TrimSpace(fromHead)+".."+strings.TrimSpace(toHead))
	if err != nil {
		return nil, fmt.Errorf("resolve files changed after test step: %w", err)
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func testStepResultIDForRun(sctx *pipeline.StepContext) (string, error) {
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return "", fmt.Errorf("load test step result for final-head verification: %w", err)
	}
	for _, step := range steps {
		if step.StepName == types.StepTest {
			return step.ID, nil
		}
	}
	return "", fmt.Errorf("final head verification required after post-test fixes, but the run has no test step result")
}

func finalHeadVerificationFindings(head string, changedFiles, tested []string, summary string, items []Finding) Findings {
	headLabel := shortObjectID(head)
	if strings.TrimSpace(summary) == "" {
		summary = fmt.Sprintf("final head %s verified after post-test changes", headLabel)
	}
	return Findings{
		Items:          items,
		Summary:        summary,
		Tested:         append([]string(nil), tested...),
		TestingSummary: fmt.Sprintf("final head %s verified after post-test changes to %s", headLabel, strings.Join(changedFiles, ", ")),
	}
}

func recordFinalHeadVerificationRound(sctx *pipeline.StepContext, testStepResultID string, findings Findings, durationMS int64) error {
	findings = types.NormalizeFindings(findings, string(types.StepTest))
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		return fmt.Errorf("marshal final-head verification findings: %w", err)
	}
	raw := string(findingsJSON)
	if err := sctx.DB.SetStepFindings(testStepResultID, raw); err != nil {
		return fmt.Errorf("record final-head verification findings: %w", err)
	}
	stats, err := sctx.DB.StepRoundStats(testStepResultID)
	if err != nil {
		return fmt.Errorf("load test round stats for final-head verification: %w", err)
	}
	if _, err := sctx.DB.InsertStepRound(testStepResultID, stats.LatestRound+1, postTestVerificationTrigger, &raw, nil, durationMS); err != nil {
		return fmt.Errorf("record final-head verification round: %w", err)
	}
	return nil
}

func joinStepNames(steps []types.StepName) string {
	if len(steps) == 0 {
		return "later-step"
	}
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		parts = append(parts, string(step))
	}
	return strings.Join(parts, "/")
}
