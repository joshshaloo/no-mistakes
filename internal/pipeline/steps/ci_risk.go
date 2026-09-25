package steps

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

var errPublishStaleRisk = errors.New("cannot publish stale review assessment")

func validationNotice(sctx *pipeline.StepContext, phase string) string {
	_, riskLine, _ := (&PRStep{}).buildPipelineSection(sctx)
	if strings.TrimSpace(riskLine) == "" {
		riskLine = "Review assessment unavailable"
	}
	supervisor := ""
	if strings.Contains(strings.ToUpper(riskLine), "STALE") {
		supervisor = "\n\nChanges made after review are not covered by the previous assessment. Get a fresh review before relying on that rating to merge; green CI does not make the previous rating current."
	}
	return fmt.Sprintf("## no-mistakes validation notice\n\n**Run:** `%s`  \n**Head:** `%s`  \n**Phase:** %s  \n**Review:** %s%s\n\nValidation notices are append-only entries in this PR conversation; the PR title, description, and human comments are not rewritten.", sctx.Run.ID, sctx.Run.HeadSHA, phase, riskLine, supervisor)
}

func publishValidationNotice(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, phase string) error {
	publisher, ok := host.(scm.PRNoticePublisher)
	if !ok {
		return errors.New("provider cannot publish validation notices")
	}
	if err := publisher.PublishPRNotice(sctx.Ctx, pr, validationNotice(sctx, phase)); err != nil {
		return fmt.Errorf("publish validation notice: %w", err)
	}
	sctx.Log(fmt.Sprintf("published head-bound validation notice in PR conversation for %s", sctx.Run.HeadSHA))
	return nil
}

// publishRiskNotice runs before a CI push and again at the CI-ready boundary.
// It appends a head-bound notice and never modifies PR metadata or prior
// comments. The ready-boundary publication is intentionally forced so a notice
// removed after the repair push cannot leave green checks beside an old rating.
func (s *CIStep) publishRiskNotice(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, force bool) error {
	stale, err := pipeline.StoredReviewRiskStale(sctx.DB, sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("%w: %w", errPublishStaleRisk, err)
	}
	if !stale || (!force && s.riskNoticeHead == sctx.Run.HeadSHA) {
		return nil
	}
	if err := publishValidationNotice(sctx, host, pr, "CI review assessment STALE"); err != nil {
		return fmt.Errorf("%w: %w", errPublishStaleRisk, err)
	}
	s.riskNoticeHead = sctx.Run.HeadSHA
	sctx.Log("risk assessment STALE: post-review changes require fresh review; green CI does not make the previous rating merge authority")
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
	return s.publishRiskNotice(sctx, host, &scm.PR{Number: number, URL: *sctx.Run.PRURL}, false)
}
