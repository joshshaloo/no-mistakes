package steps

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const postTestVerificationTrigger = "post_test_fix_verification"

// recordPostTestHeadAdvance is the single registration point for "a pipeline
// step advanced HEAD after the Test step". Every local commit point between
// Test and the network push routes through it: the document and lint fix
// rounds via commitAgentFixes, and the push step's own format-and-commit path.
//
// The marker it feeds is in-memory and therefore an optimization only. The
// authority the push boundary actually consults is the run's durable
// test-verified head anchor, which survives a daemon restart; this marker just
// names the steps involved for the log line.
func recordPostTestHeadAdvance(sctx *pipeline.StepContext, step types.StepName, fromHead, toHead string) {
	if step.Order() <= types.StepTest.Order() || step.Order() > types.StepPush.Order() {
		return
	}
	sctx.Shared.MarkPostTestFixCommit(step, fromHead, toHead)
}

// verifyFinalHeadAfterPostTestFixes is the single owner for the rule that a
// pipeline-owned commit made by a later local step invalidates the earlier Test
// result. The push boundary calls it after all local mutation/commit points and
// before the network push, so the run's recorded test evidence describes the
// exact head being shipped.
//
// It is anchored on the run's durable test-verified tree rather than on
// in-memory state, so a run resumed after a parked fix round still re-verifies.
// Comparing content rather than commit identity is what makes the boundary
// cheap in the common case: the test agent's own new test files and the
// pipeline's staged evidence artifacts are already inside the validated tree
// when a later step commits them, so landing them is not a post-test change,
// while any other edit to tracked content is. A tree that cannot be resolved
// counts as changed, never as unchanged.
//
// When the Test step produced no green evidence (skipped, failed, approved with
// failures, or absent) there is nothing to invalidate: the boundary records the
// concrete reason and no-ops instead of manufacturing evidence or blocking.
func verifyFinalHeadAfterPostTestFixes(sctx *pipeline.StepContext) error {
	anchor, err := resolveTestVerifiedAnchor(sctx)
	if err != nil {
		return err
	}
	if anchor.reason != "" {
		sctx.Log(fmt.Sprintf("skipping final head verification: %s", anchor.reason))
		sctx.Shared.CompletePostTestFixVerification()
		return nil
	}
	currentHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("resolve final head for post-test verification: %w", err)
	}
	shippedTree, treeErr := git.HeadTreeSHA(sctx.Ctx, sctx.WorkDir)
	if treeErr != nil {
		shippedTree = ""
		sctx.Log(fmt.Sprintf("could not resolve the final tree (%v), re-verifying the final head", treeErr))
	}
	if shippedTree != "" && shippedTree == anchor.treeSHA {
		sctx.Shared.CompletePostTestFixVerification()
		return nil
	}
	changedFiles := postTestChangedFiles(sctx, anchor.treeSHA, shippedTree)

	started := time.Now()
	steps := joinStepNames(pendingPostTestSteps(sctx))
	headLabel := shortObjectID(currentHead)

	testCmd := ""
	if sctx.Config != nil {
		testCmd = strings.TrimSpace(sctx.Config.Commands.Test)
	}
	if testCmd != "" {
		sctx.Log(fmt.Sprintf("re-running test verification on final head %s after post-test %s changes: %s", headLabel, steps, testCmd))
		output, exitCode, err := runConfiguredStepShellCommand(sctx, configuredCommandTest, testCmd)
		tested := []string{testCmd}
		projectedOutput := logConfiguredCommandOutput(sctx, output, types.StepPush)
		if err != nil {
			findings := finalHeadVerificationFindings(currentHead, changedFiles, tested, err.Error(), []Finding{{
				Severity:    "error",
				Action:      types.ActionAutoFix,
				Description: err.Error(),
			}})
			if recordErr := recordFinalHeadVerificationRound(sctx, anchor.stepResultID, findings, time.Since(started).Milliseconds()); recordErr != nil {
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
			if recordErr := recordFinalHeadVerificationRound(sctx, anchor.stepResultID, findings, time.Since(started).Milliseconds()); recordErr != nil {
				return fmt.Errorf("record final head verification failure: %w (original failure: %s)", recordErr, description)
			}
			return fmt.Errorf("final head verification failed after post-test fixes: %s", description)
		}
		findings := finalHeadVerificationFindings(currentHead, changedFiles, tested, "", nil)
		if err := recordFinalHeadVerificationRound(sctx, anchor.stepResultID, findings, time.Since(started).Milliseconds()); err != nil {
			return err
		}
		return completeFinalHeadVerification(sctx, shippedTree)
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
	verification := finalHeadVerificationFindings(currentHead, changedFiles, findings.Tested, findings.Summary, findings.Items)
	verification.Artifacts = findings.Artifacts
	if len(verification.Tested) == 0 {
		verification.Items = append(verification.Items, Finding{
			Severity:    "error",
			Action:      types.ActionAutoFix,
			Description: "final head verification did not report any test, typecheck, or manual verification command",
		})
	}
	if err := recordFinalHeadVerificationRound(sctx, anchor.stepResultID, verification, time.Since(started).Milliseconds()); err != nil {
		return err
	}
	if hasBlockingFindings(verification.Items) {
		return fmt.Errorf("final head verification failed after post-test fixes: %s", verification.Summary)
	}
	return completeFinalHeadVerification(sctx, shippedTree)
}

// testVerifiedAnchor is the durable proof of which content the run's recorded
// test evidence describes. A non-empty reason means there is no such proof and
// the boundary must no-op rather than invent one.
type testVerifiedAnchor struct {
	treeSHA      string
	stepResultID string
	reason       string
}

func resolveTestVerifiedAnchor(sctx *pipeline.StepContext) (testVerifiedAnchor, error) {
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return testVerifiedAnchor{}, fmt.Errorf("load test step result for final-head verification: %w", err)
	}
	var testStep *db.StepResult
	for _, step := range steps {
		if step.StepName == types.StepTest {
			testStep = step
			break
		}
	}
	if testStep == nil {
		return testVerifiedAnchor{reason: "the run has no test step result"}, nil
	}
	if testStep.Status != types.StepStatusCompleted {
		return testVerifiedAnchor{reason: fmt.Sprintf("the test step is %s, so there is no green evidence to invalidate", testStep.Status)}, nil
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return testVerifiedAnchor{}, fmt.Errorf("load durable test-verified tree for final-head verification: %w", err)
	}
	if run == nil || run.TestVerifiedTreeSHA == nil || strings.TrimSpace(*run.TestVerifiedTreeSHA) == "" {
		return testVerifiedAnchor{reason: "the test step recorded no verified tree, so there is no green evidence to invalidate"}, nil
	}
	return testVerifiedAnchor{treeSHA: strings.TrimSpace(*run.TestVerifiedTreeSHA), stepResultID: testStep.ID}, nil
}

// completeFinalHeadVerification moves the durable anchor onto the tree just
// verified, so a resumed or retried push boundary does not re-run the same
// verification for content that already carries fresh evidence. An unresolved
// tree advances nothing, leaving the next boundary to re-verify.
func completeFinalHeadVerification(sctx *pipeline.StepContext, verifiedTree string) error {
	if verifiedTree == "" {
		sctx.Shared.CompletePostTestFixVerification()
		return nil
	}
	if err := sctx.DB.UpdateRunTestVerifiedTreeSHA(sctx.Run.ID, verifiedTree); err != nil {
		return fmt.Errorf("record verified final tree: %w", err)
	}
	verified := verifiedTree
	sctx.Run.TestVerifiedTreeSHA = &verified
	sctx.Shared.CompletePostTestFixVerification()
	return nil
}

func pendingPostTestSteps(sctx *pipeline.StepContext) []types.StepName {
	change, ok := sctx.Shared.PendingPostTestFixChange()
	if !ok {
		return nil
	}
	return change.Steps
}

// postTestChangedFiles names the tracked content that changed since the tests
// ran. It is descriptive input for the verification prompt and summary, not the
// predicate that decides whether to verify, so an unresolved tree or a failed
// diff degrades to an explicit placeholder rather than an empty "nothing
// changed" list.
func postTestChangedFiles(sctx *pipeline.StepContext, fromTree, toTree string) []string {
	unknown := []string{"(files changed after the test step could not be determined)"}
	if strings.TrimSpace(toTree) == "" {
		return unknown
	}
	out, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-only", strings.TrimSpace(fromTree), strings.TrimSpace(toTree))
	if err != nil {
		return unknown
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	if len(files) == 0 {
		return unknown
	}
	return files
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

// recordFinalHeadVerificationRound merges the verification into the Test step's
// stored findings. The Test step's evidence - artifacts, tested entries, and
// testing summary - is what the PR body renders, so it is never replaced: the
// verification is appended as its own entry alongside it.
func recordFinalHeadVerificationRound(sctx *pipeline.StepContext, testStepResultID string, verification Findings, durationMS int64) error {
	verification = types.NormalizeFindings(verification, string(types.StepTest))
	verificationJSON, err := json.Marshal(verification)
	if err != nil {
		return fmt.Errorf("marshal final-head verification findings: %w", err)
	}
	existing, err := loadStepFindings(sctx, testStepResultID)
	if err != nil {
		return err
	}
	mergedJSON, err := json.Marshal(mergeFinalHeadVerificationFindings(existing, verification))
	if err != nil {
		return fmt.Errorf("marshal merged test findings: %w", err)
	}
	if err := sctx.DB.SetStepFindings(testStepResultID, string(mergedJSON)); err != nil {
		return fmt.Errorf("record final-head verification findings: %w", err)
	}
	stats, err := sctx.DB.StepRoundStats(testStepResultID)
	if err != nil {
		return fmt.Errorf("load test round stats for final-head verification: %w", err)
	}
	raw := string(verificationJSON)
	if _, err := sctx.DB.InsertStepRound(testStepResultID, stats.LatestRound+1, postTestVerificationTrigger, &raw, nil, durationMS); err != nil {
		return fmt.Errorf("record final-head verification round: %w", err)
	}
	return nil
}

func loadStepFindings(sctx *pipeline.StepContext, stepResultID string) (Findings, error) {
	step, err := sctx.DB.GetStepResult(stepResultID)
	if err != nil {
		return Findings{}, fmt.Errorf("load recorded test findings: %w", err)
	}
	if step == nil || step.FindingsJSON == nil || strings.TrimSpace(*step.FindingsJSON) == "" {
		return Findings{}, nil
	}
	existing, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		// Unreadable prior evidence is preserved by leaving it out of the merge
		// rather than being rewritten into a shape it never had; the durable
		// round records still carry the original bytes.
		return Findings{}, nil
	}
	return existing, nil
}

// mergeFinalHeadVerificationFindings keeps every piece of the Test step's own
// evidence and adds the verification to it. Items are appended so a step whose
// only findings were informational never renders as "fixed", tested entries and
// artifacts accumulate, and the testing summary gains the verification as an
// additional sentence instead of losing the original.
func mergeFinalHeadVerificationFindings(existing, verification Findings) Findings {
	merged := existing
	// Both sets were normalized independently, so the verification's IDs are
	// index-based duplicates of the existing ones. Drop them and let the
	// findings-ID owner reassign collision-free IDs across the merged set.
	appended := make([]Finding, 0, len(verification.Items))
	for _, item := range verification.Items {
		item.ID = ""
		appended = append(appended, item)
	}
	merged.Items = append(append([]Finding(nil), existing.Items...), appended...)
	merged.Tested = appendUniqueStrings(existing.Tested, verification.Tested)
	merged.Artifacts = appendUniqueArtifacts(existing.Artifacts, verification.Artifacts)
	merged.TestingSummary = appendSummarySentence(existing.TestingSummary, verification.TestingSummary)
	merged.Summary = appendSummarySentence(existing.Summary, verification.Summary)
	return types.NormalizeFindings(merged, string(types.StepTest))
}

func appendUniqueStrings(existing, added []string) []string {
	seen := make(map[string]bool, len(existing))
	result := append([]string(nil), existing...)
	for _, value := range existing {
		seen[value] = true
	}
	for _, value := range added {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func appendUniqueArtifacts(existing, added []types.TestArtifact) []types.TestArtifact {
	key := func(a types.TestArtifact) string {
		return strings.Join([]string{a.Kind, a.Label, a.Path, a.URL, a.Content}, "\x00")
	}
	seen := make(map[string]bool, len(existing))
	result := append([]types.TestArtifact(nil), existing...)
	for _, artifact := range existing {
		seen[key(artifact)] = true
	}
	for _, artifact := range added {
		if seen[key(artifact)] {
			continue
		}
		seen[key(artifact)] = true
		result = append(result, artifact)
	}
	return result
}

func appendSummarySentence(existing, added string) string {
	existing = strings.TrimSpace(existing)
	added = strings.TrimSpace(added)
	switch {
	case added == "":
		return existing
	case existing == "":
		return added
	case strings.Contains(existing, added):
		return existing
	default:
		return existing + "\n\n" + added
	}
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
