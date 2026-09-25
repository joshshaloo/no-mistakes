package steps

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

var errPublishStaleRisk = errors.New("cannot publish stale review assessment")

func validationNotice(sctx *pipeline.StepContext, phase string) (string, error) {
	_, riskLine, _, err := (&PRStep{}).buildPipelineSectionStrict(sctx)
	if err != nil {
		return "", fmt.Errorf("read validation notice evidence: %w", err)
	}
	if strings.TrimSpace(riskLine) == "" {
		riskLine = "Review assessment unavailable"
	}
	supervisor := ""
	if strings.Contains(strings.ToUpper(riskLine), "STALE") {
		supervisor = "\n\nChanges made after review are not covered by the previous assessment. Get a fresh review before relying on that rating to merge; green CI does not make the previous rating current."
	}
	return fmt.Sprintf("<!-- no-mistakes-validation run=%s head=%s -->\n## no-mistakes validation notice\n\n**Run:** `%s`  \n**Head:** `%s`  \n**Phase:** %s  \n**Review:** %s%s\n\nValidation notices are append-only entries in this PR conversation; the PR title, description, and human comments are not rewritten.", sctx.Run.ID, sctx.Run.HeadSHA, sctx.Run.ID, sctx.Run.HeadSHA, phase, riskLine, supervisor), nil
}

func publishValidationNotice(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, phase string) error {
	body, err := validationNotice(sctx, phase)
	if err != nil {
		return err
	}
	return publishValidationNoticeBody(sctx, host, pr, body)
}

func publishValidationNoticeBody(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, body string) error {
	publisher, ok := host.(scm.PRNoticePublisher)
	if !ok {
		return errors.New("provider cannot publish validation notices")
	}
	if err := publisher.PublishPRNotice(sctx.Ctx, pr, body); err != nil {
		return fmt.Errorf("publish validation notice: %w", err)
	}
	sctx.Log(fmt.Sprintf("published head-bound validation notice in PR conversation for %s", sctx.Run.HeadSHA))
	return nil
}

func (s *CIStep) reconcileRiskNotice(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, phase string) error {
	stale, err := pipeline.StoredReviewRiskStale(sctx.DB, sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("%w: read stored review risk: %w", errPublishStaleRisk, err)
	}
	if !stale {
		return nil
	}
	buildNotice := validationNotice
	if s.buildValidationNotice != nil {
		buildNotice = s.buildValidationNotice
	}
	body, err := buildNotice(sctx, phase)
	if err != nil {
		return fmt.Errorf("%w: %w", errPublishStaleRisk, err)
	}
	reader, ok := host.(scm.PRNoticeReader)
	if !ok {
		return fmt.Errorf("%w: provider cannot read validation notices", errPublishStaleRisk)
	}
	notices, err := reader.ListPRNotices(sctx.Ctx, pr)
	if err != nil {
		return fmt.Errorf("%w: read PR validation notices: %w", errPublishStaleRisk, err)
	}
	marker := fmt.Sprintf("<!-- no-mistakes-validation run=%s head=%s -->", sctx.Run.ID, sctx.Run.HeadSHA)
	latest := ""
	for _, notice := range notices {
		if strings.HasPrefix(notice, marker) {
			latest = notice
		}
	}
	if latest == body {
		return nil
	}
	if err := publishValidationNoticeBody(sctx, host, pr, body); err != nil {
		return fmt.Errorf("%w: %w", errPublishStaleRisk, err)
	}
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
	return s.reconcileRiskNotice(sctx, host, &scm.PR{Number: number, URL: *sctx.Run.PRURL}, "CI repair push pending")
}
