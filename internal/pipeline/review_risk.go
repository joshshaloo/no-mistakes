package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ErrReviewRisk marks a failure to persist or read assessment freshness. A CI
// repair must not swallow it as an ordinary unsuccessful repair attempt.
var ErrReviewRisk = errors.New("cannot establish review risk freshness")

// StoredReviewRiskStale reads the durable verdict without opening the worktree.
// Read-only CI polls and recovered monitors consume this; only mutation
// boundaries call RefreshReviewRisk. A poll must not repeat Git verification or
// move that work into the terminal path before cleanup.
func StoredReviewRiskStale(database *db.DB, runID string) (bool, error) {
	if database == nil {
		return false, fmt.Errorf("%w: missing run database", ErrReviewRisk)
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrReviewRisk, err)
	}
	for _, sr := range steps {
		if sr.StepName != types.StepReview || sr.Status != types.StepStatusCompleted || sr.FindingsJSON == nil {
			continue
		}
		findings, err := types.ParseFindingsJSON(*sr.FindingsJSON)
		if err != nil {
			return false, fmt.Errorf("%w: %w", ErrReviewRisk, err)
		}
		return findings.RiskLevel == types.RiskStale, nil
	}
	return false, nil
}

// RefreshReviewRisk invalidates the current assessment, not the historical
// review rounds or the ancestry approval used by Push. The exact completed
// review head is the authority; neither a later head nor a green CI check can
// renew it. An empty target compares tracked worktree content (including staged
// additions); a commit target is checked before publication/push. Once stale,
// only another completed full review can supply a fresh assessment.
//
// The exemption is deliberately narrow: non-executable plain prose under docs/
// or a few conventional root documents. Tests, configuration, agent instructions,
// MDX, scripts, symlinks and mode changes are NOT mechanical documentation.
// Unreadable evidence fails safe to stale, never to the old low/medium rating.
func RefreshReviewRisk(ctx context.Context, database *db.DB, runID, workDir, target string, onInvalidated func(string)) (stale bool, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrReviewRisk, err)
		}
	}()
	if database == nil {
		return false, errors.New("missing run database")
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return false, err
	}
	for _, sr := range steps {
		if sr.StepName != types.StepReview || sr.Status != types.StepStatusCompleted {
			continue
		}
		var findings types.Findings
		if sr.FindingsJSON != nil {
			findings, err = types.ParseFindingsJSON(*sr.FindingsJSON)
			if err != nil {
				return false, fmt.Errorf("read review risk: %w", err)
			}
		} else {
			// Some legacy completions clear final findings; their presenters fall back
			// to the latest round, so invalidate that assessment too, without rewriting
			// its historical record.
			rounds, err := database.GetRoundsByStep(sr.ID)
			if err != nil {
				return false, err
			}
			if len(rounds) == 0 || rounds[len(rounds)-1].FindingsJSON == nil {
				continue
			}
			findings, err = types.ParseFindingsJSON(*rounds[len(rounds)-1].FindingsJSON)
			if err != nil {
				return false, fmt.Errorf("read review round risk: %w", err)
			}
		}
		if findings.RiskLevel == types.RiskStale {
			return true, nil
		}
		if findings.RiskLevel == "" {
			continue
		}
		run, err := database.GetRun(runID)
		if err != nil {
			return false, err
		}
		current := false
		if run.ReviewApprovedHeadSHA != nil && *run.ReviewApprovedHeadSHA != "" {
			args := []string{"diff", "--raw", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none", *run.ReviewApprovedHeadSHA}
			if target != "" {
				args = append(args, target)
			}
			args = append(args, "--")
			diff, err := git.Run(ctx, workDir, args...)
			current = err == nil && onlyMechanicalDocumentation(diff)
		}
		if current {
			return false, nil
		}
		findings.RiskLevel = types.RiskStale
		findings.RiskRationale = types.StaleRiskRationale
		findings.RiskScope = ""
		raw, err := types.MarshalFindingsJSON(findings)
		if err != nil {
			return false, err
		}
		if err := database.SetStepFindings(sr.ID, raw); err != nil {
			return false, fmt.Errorf("invalidate review risk: %w", err)
		}
		if onInvalidated != nil {
			onInvalidated(raw)
		}
		return true, nil
	}
	return false, nil
}

func onlyMechanicalDocumentation(raw string) bool {
	if raw == "" {
		return true
	}
	records := strings.Split(raw, "\x00")
	if records[len(records)-1] != "" {
		return false
	}
	records = records[:len(records)-1]
	if len(records)%2 != 0 {
		return false
	}
	for i := 0; i < len(records); i += 2 {
		header := strings.Fields(records[i])
		if len(header) != 5 || !strings.HasPrefix(header[0], ":") {
			return false
		}
		oldMode, newMode := strings.TrimPrefix(header[0], ":"), header[1]
		switch header[4] {
		case "A":
			if oldMode != "000000" || newMode != "100644" {
				return false
			}
		case "D":
			if oldMode != "100644" || newMode != "000000" {
				return false
			}
		case "M":
			if oldMode != "100644" || newMode != "100644" {
				return false
			}
		default:
			return false
		}
		if !plainDocumentationPath(records[i+1]) {
			return false
		}
	}
	return true
}

func plainDocumentationPath(name string) bool {
	switch path.Base(name) {
	case "AGENTS.md", "CLAUDE.md", "SKILL.md":
		return false
	}
	switch name {
	case "README.md", "CONTRIBUTING.md", "CHANGELOG.md":
		return true
	}
	if !strings.HasPrefix(name, "docs/") {
		return false
	}
	switch path.Ext(name) {
	case ".md", ".txt", ".rst":
		return true
	}
	return false
}
