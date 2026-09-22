package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/kunchenguid/no-mistakes/internal/convergence"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// nonconvergenceStep is the step whose rounds carry the judgment. Review is the
// step whose rounds are a cause being diagnosed and patched, so it is the only
// one where "did the earlier fixes settle the cause" is a meaningful question.
// The detector itself takes a step name, so widening this later is wiring, not
// a redesign.
const nonconvergenceStep = types.StepReview

// observeNonconvergence asks, after a review round beyond the first, whether
// the fixes already applied on this run have failed to settle the underlying
// cause behind the current round's findings, and records the probability on
// the run.
//
// It is a SIGNAL, never a gate: it returns nothing, it cannot fail the step,
// and every path through it leaves the run's control flow byte-identical to a
// build without it. On any problem - detector off, history unreadable, call
// failed, answer missing - it records nothing and moves on. See
// internal/convergence's package doc for why the failure direction is inverted
// here relative to a guard, and why that pattern must not be copied into one.
func (e *Executor) observeNonconvergence(ctx context.Context, runID, stepResultID string, stepName types.StepName, intent string, writeLog func(string)) {
	if e.convergence == nil || stepName != nonconvergenceStep {
		return
	}
	rounds, err := e.db.GetRoundsByStep(stepResultID)
	if err != nil || len(rounds) < 2 {
		return
	}

	obs := convergence.Observation{
		Intent:  intent,
		Step:    string(stepName),
		Current: convergenceRound(rounds[len(rounds)-1]),
	}
	for _, r := range rounds[:len(rounds)-1] {
		obs.Earlier = append(obs.Earlier, convergenceRound(r))
	}

	sig, err := e.convergence.Observe(ctx, obs)
	if err != nil {
		// One informational line makes a permanently rejected response shape
		// visible without turning this optional signal into a pipeline alarm.
		// Redacted defensively so a credentialled endpoint wrapped into the
		// error can never reach a log.
		slog.Info("non-convergence signal unavailable", "run", runID, "step", stepName, "error", safeurl.RedactText(err.Error()))
		return
	}
	if sig == nil {
		return
	}

	record := db.Nonconvergence{
		Probability: sig.Probability,
		Model:       sig.Model,
		Step:        sig.Step,
		Round:       sig.Round,
		Themes:      sig.CausalThemes,
		ObservedAt:  sig.ObservedAt,
	}
	if dbErr := e.db.RecordRunNonconvergence(runID, record); dbErr != nil {
		slog.Debug("failed to record non-convergence signal", "run", runID, "step", stepName, "error", dbErr)
		return
	}
	if writeLog != nil {
		writeLog(formatNonconvergenceLog(record))
	}
}

// formatNonconvergenceLog renders the signal for the step log. It names the
// signal as a signal so nobody reads it as a verdict the pipeline acted on.
func formatNonconvergenceLog(nc db.Nonconvergence) string {
	line := fmt.Sprintf("non-convergence signal after round %d: p=%.2f (model %s)", nc.Round, nc.Probability, nc.Model)
	if nc.Themes != nil {
		line += fmt.Sprintf(", causal themes %.2f", *nc.Themes)
	}
	return line + " - signal only, nothing in this run changed"
}

// convergenceRound maps one persisted round onto the detector's input. Only
// finding metadata and the one-line fix summary travel; no diff, patch, or file
// content is ever sent.
func convergenceRound(r *db.StepRound) convergence.Round {
	out := convergence.Round{Number: r.Round}
	if r.Trigger != "" {
		out.Trigger = r.Trigger
		if r.IsFixRound() {
			out.Trigger = "auto_fix"
		}
	}
	if r.FixSummary != nil {
		out.FixSummary = *r.FixSummary
	}
	// What the round reported, plus any finding the user authored on top of it.
	// The reported list is the base rather than the dispatched list, so a
	// finding the user chose to ignore stays visible: a cause that keeps
	// resurfacing while nobody fixes it is exactly what the question is about.
	seen := map[string]bool{}
	appendFindings := func(raw *string) {
		if raw == nil || *raw == "" {
			return
		}
		parsed, err := types.ParseFindingsJSON(*raw)
		if err != nil {
			return
		}
		for _, f := range parsed.Items {
			if f.ID != "" {
				if seen[f.ID] {
					continue
				}
				seen[f.ID] = true
			}
			out.Findings = append(out.Findings, convergence.Finding{
				ID:          f.ID,
				Severity:    f.Severity,
				File:        f.File,
				Action:      f.Action,
				Description: f.Description,
			})
		}
	}
	appendFindings(r.FindingsJSON)
	appendFindings(r.UserFindingsJSON)
	if r.SelectedFindingIDs != nil && *r.SelectedFindingIDs != "" {
		var ids []string
		if err := json.Unmarshal([]byte(*r.SelectedFindingIDs), &ids); err == nil {
			out.SelectedForFix = ids
		}
	}
	return out
}
