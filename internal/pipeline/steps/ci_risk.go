package steps

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var errPublishStaleRisk = errors.New("cannot publish stale review assessment")

const staleRiskNotice = "> ⚠️ **Risk assessment: STALE.** " + types.StaleRiskRationale + "\n\n"

// publishRiskNotice runs before a CI push and before reporting checks green,
// including after monitor recovery. Unlike ordinary best-effort PR cosmetics,
// failure here must stop the run: the remote must not keep advertising LOW for
// code we are about to push. Only generated sections are replaced; title and
// human-authored summary are fetched and preserved. No extra agent is invoked.
func (s *CIStep) publishRiskNotice(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) error {
	stale, err := pipeline.StoredReviewRiskStale(sctx.DB, sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("%w: %w", errPublishStaleRisk, err)
	}
	if !stale || s.riskNoticeHead == sctx.Run.HeadSHA {
		return nil
	}
	reader, ok := host.(scm.PRContentReader)
	if !ok {
		return fmt.Errorf("%w: provider cannot read PR content", errPublishStaleRisk)
	}
	content, err := reader.GetPRContent(sctx.Ctx, pr)
	if err != nil {
		return fmt.Errorf("%w: %w", errPublishStaleRisk, err)
	}
	if strings.TrimSpace(content.Title) == "" {
		return fmt.Errorf("%w: PR title unavailable", errPublishStaleRisk)
	}
	pipelineMD, _, testingMD := (&PRStep{}).buildPipelineSection(sctx)
	body := strings.TrimPrefix(content.Body, staleRiskNotice)
	content.Body = staleRiskNotice + appendGeneratedSections(body, "⚠️ STALE: "+types.StaleRiskRationale, testingMD, pipelineMD)
	content.Body = scm.ClampPRBody(content.Body, scm.MaxPRBodyChars(host.Provider()))
	if _, err := host.UpdatePR(sctx.Ctx, pr, content); err != nil {
		return fmt.Errorf("%w: %w", errPublishStaleRisk, err)
	}
	s.riskNoticeHead = sctx.Run.HeadSHA
	sctx.Log("risk assessment STALE: post-review changes require review; previous rating is not merge authority")
	return nil
}

func (s *CIStep) publishRiskNoticeBeforePush(sctx *pipeline.StepContext) error {
	if sctx.Run.PRURL == nil || strings.TrimSpace(*sctx.Run.PRURL) == "" {
		return nil
	}
	provider := scm.DetectProviderContext(sctx.Ctx, *sctx.Run.PRURL)
	host, reason := buildHost(sctx, provider)
	if host == nil {
		return fmt.Errorf("%w: %s", errPublishStaleRisk, reason)
	}
	number, err := scm.ExtractPRNumber(*sctx.Run.PRURL)
	if err != nil {
		return fmt.Errorf("%w: %w", errPublishStaleRisk, err)
	}
	return s.publishRiskNotice(sctx, host, &scm.PR{Number: number, URL: *sctx.Run.PRURL})
}
