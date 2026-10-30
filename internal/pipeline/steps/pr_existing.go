package steps

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// An unconfigured existing PR remains author-owned. The only body mutation is
// upstream's exact, already-present v1 attestation restamp, which is required
// before publication; no narrative or title is regenerated. Explicit templates
// keep upstream's opt-in owned-appendix behavior.
func (s *PRStep) reuseExistingPR(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) (*pipeline.StepOutcome, error) {
	if err := retargetExistingPRIfNeeded(sctx, host, pr, runPRBaseBranch(sctx)); err != nil {
		return nil, err
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return nil, err
	}
	if err := restampPRAttestationWithSteps(sctx.Ctx, host, pr, sctx.Run.HeadSHA, steps, sctx.Log, attestationPolicyFrom(sctx)); err != nil {
		return nil, err
	}
	sctx.Log(fmt.Sprintf("pull request already exists: %s; preserving its title and narrative", describePR(pr)))
	if err := publishSupportedValidationNotice(sctx, host, pr, "PR step"); err != nil {
		return nil, err
	}
	if pr.URL != "" {
		if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, pr.URL); err != nil {
			return nil, err
		}
	}
	return &pipeline.StepOutcome{PRURL: pr.URL}, nil
}

func publishSupportedValidationNotice(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, phase string) error {
	// The fork's original four providers have a verified notice contract. New
	// upstream providers retain their existing behavior until they expose it.
	switch host.Provider() {
	case scm.ProviderGitHub, scm.ProviderGitLab, scm.ProviderBitbucket, scm.ProviderAzureDevOps:
		return publishValidationNotice(sctx, host, pr, phase, "")
	default:
		return nil
	}
}
