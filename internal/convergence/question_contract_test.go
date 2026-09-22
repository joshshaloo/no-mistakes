package convergence

import (
	"strings"
	"testing"
)

// The question wording is load-bearing, not decorative. Each clause pinned here
// was the difference between separating real non-convergence from mere
// similarity and not separating it at all, measured against real multi-round
// runs: same-subsystem independent gaps 0.11, an independent follow-up 0.25,
// the same class at a sibling site after a point fix 0.75, and a previous fix
// that caused the current finding 0.91. Changing any of these means re-tuning
// against cases like those, not editing prose.
func TestNonConvergenceQuestionKeepsItsDiscriminatingClauses(t *testing.T) {
	for _, want := range []string{
		// Convergence, not similarity, is the whole distinction being asked for.
		"Judge convergence, not similarity",
		// A point patch that leaves the cause reachable is the central case.
		"repaired only the exact site it was reported at",
		// The question is asked over the run's own history, by field name.
		"`earlier_rounds`",
		"`current_round`",
	} {
		if !strings.Contains(nonConvergenceInstructions, want) {
			t.Errorf("non-convergence instructions lost %q:\n%s", want, nonConvergenceInstructions)
		}
	}

	// The three causal links that make an answer true.
	for _, want := range []string{
		"invariant an earlier fix was meant to establish is violated again",
		"same class of defect reappears at a sibling site",
		"previous fix on that cause is what produced the current finding",
	} {
		if !strings.Contains(nonConvergenceTrue, want) {
			t.Errorf("true criterion lost %q:\n%s", want, nonConvergenceTrue)
		}
	}

	// Without this exclusion, several independent gaps in one subsystem read as
	// non-convergence, which is exactly the false positive the brief rules out.
	for _, want := range []string{
		"independent gap that the earlier fixes were never about",
		"merely share a file, package, subsystem, or general topic",
	} {
		if !strings.Contains(nonConvergenceFalse, want) {
			t.Errorf("false criterion lost %q:\n%s", want, nonConvergenceFalse)
		}
	}
}

// Score criteria must stay an ordered array of concrete, self-standing levels;
// an object returns 400 invalid_type, and the API accepts 2 to 10 levels.
func TestCausalThemeScoreLevelsStayAnOrderedArray(t *testing.T) {
	if len(causalThemeLevels) < 2 || len(causalThemeLevels) > 10 {
		t.Fatalf("score needs 2-10 levels, got %d", len(causalThemeLevels))
	}
	for i, level := range causalThemeLevels {
		if strings.TrimSpace(level) == "" {
			t.Errorf("level %d is empty; levels must describe concrete situations and stand on their own", i)
		}
	}
	if !strings.Contains(causalThemesInstructions, "Count causes, not findings") {
		t.Errorf("the theme question must count causes, not findings:\n%s", causalThemesInstructions)
	}
}

// The state field names are part of the question: the instructions reference
// them by name, so renaming one silently breaks the judgment.
func TestStateFieldNamesMatchTheQuestion(t *testing.T) {
	encoded, err := buildState(twoRoundObservation())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"current_round"`, `"earlier_rounds"`, `"accepted_intent"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("state is missing field %s:\n%s", want, encoded)
		}
	}
}
