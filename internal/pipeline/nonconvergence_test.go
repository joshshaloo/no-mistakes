package pipeline

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func strPtr(s string) *string { return &s }

func TestConvergenceRoundCarriesFindingsFixSummaryAndSelection(t *testing.T) {
	round := &db.StepRound{
		Round:              2,
		Trigger:            "auto_fix",
		FixSummary:         strPtr("add process group"),
		FindingsJSON:       strPtr(`{"findings":[{"id":"f1","severity":"error","file":"a.go","action":"auto-fix","description":"leaks"},{"id":"f2","severity":"info","description":"nit"}]}`),
		SelectedFindingIDs: strPtr(`["f1"]`),
	}

	got := convergenceRound(round)
	if got.Number != 2 || got.Trigger != "auto_fix" {
		t.Errorf("round/trigger = %d/%q", got.Number, got.Trigger)
	}
	if got.FixSummary != "add process group" {
		t.Errorf("fix summary = %q", got.FixSummary)
	}
	if len(got.Findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(got.Findings))
	}
	if got.Findings[0].ID != "f1" || got.Findings[0].File != "a.go" || got.Findings[0].Action != "auto-fix" {
		t.Errorf("first finding = %+v", got.Findings[0])
	}
	if len(got.SelectedForFix) != 1 || got.SelectedForFix[0] != "f1" {
		t.Errorf("selected = %v, want [f1]", got.SelectedForFix)
	}
}

// A finding the user chose to ignore must stay visible: a cause that keeps
// resurfacing while nobody fixes it is exactly what the question is about.
func TestConvergenceRoundKeepsIgnoredFindingsAndAddsUserAuthoredOnes(t *testing.T) {
	round := &db.StepRound{
		Round:              3,
		Trigger:            "auto_fix",
		FindingsJSON:       strPtr(`{"findings":[{"id":"f1","description":"reported and fixed"},{"id":"f2","description":"reported and ignored"}]}`),
		UserFindingsJSON:   strPtr(`{"findings":[{"id":"f1","description":"reported and fixed"},{"id":"u1","description":"user added this"}]}`),
		SelectedFindingIDs: strPtr(`["f1","u1"]`),
	}

	got := convergenceRound(round)
	ids := map[string]bool{}
	for _, f := range got.Findings {
		if ids[f.ID] {
			t.Fatalf("finding %q appears twice", f.ID)
		}
		ids[f.ID] = true
	}
	for _, want := range []string{"f1", "f2", "u1"} {
		if !ids[want] {
			t.Errorf("finding %q missing from %v", want, ids)
		}
	}
}

func TestConvergenceRoundToleratesMalformedPersistedJSON(t *testing.T) {
	got := convergenceRound(&db.StepRound{
		Round:              4,
		Trigger:            "initial",
		FindingsJSON:       strPtr("{not json"),
		SelectedFindingIDs: strPtr("[not json"),
	})
	if len(got.Findings) != 0 || len(got.SelectedForFix) != 0 {
		t.Fatalf("malformed history must yield nothing, got %+v", got)
	}
	if got.Number != 4 {
		t.Errorf("round number must survive, got %d", got.Number)
	}
}

func TestConvergenceRoundNormalizesLegacyUserFixTrigger(t *testing.T) {
	got := convergenceRound(&db.StepRound{Round: 2, Trigger: "user_fix"})
	if got.Trigger != "auto_fix" {
		t.Fatalf("legacy user_fix rounds are fix rounds, got %q", got.Trigger)
	}
}

func TestFormatNonconvergenceLogNamesItAsASignal(t *testing.T) {
	themes := 1.2
	line := formatNonconvergenceLog(db.Nonconvergence{Probability: 0.88, Model: "jev-x", Round: 3, Themes: &themes})
	for _, want := range []string{"non-convergence signal", "round 3", "p=0.88", "jev-x", "causal themes 1.20", "nothing in this run changed"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q missing %q", line, want)
		}
	}
	withoutThemes := formatNonconvergenceLog(db.Nonconvergence{Probability: 0.1, Model: "jev-x", Round: 2})
	if strings.Contains(withoutThemes, "causal themes") {
		t.Errorf("an absent score must not be rendered: %q", withoutThemes)
	}
}
