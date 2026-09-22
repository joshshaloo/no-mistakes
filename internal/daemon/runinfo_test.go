package daemon

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunToInfoIncludesImmutableSubmittedHead(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "submitted-head", "base-head")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, "pipeline-fix-head"); err != nil {
		t.Fatalf("advance run head: %v", err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}

	info := runToInfo(d, run, nil)
	if info.HeadSHA != "pipeline-fix-head" {
		t.Fatalf("head = %q, want pipeline-fix-head", info.HeadSHA)
	}
	if info.SubmittedHeadSHA == nil || *info.SubmittedHeadSHA != "submitted-head" {
		t.Fatalf("submitted head = %v, want submitted-head", info.SubmittedHeadSHA)
	}
}

func TestStepToInfoIncludesFixSummaries(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}

	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"x"}],"summary":"1"}`
	if _, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 100); err != nil {
		t.Fatalf("insert round 1: %v", err)
	}
	sum := "handle nil pointer in executor"
	if _, err := d.InsertStepRound(step.ID, 2, "auto_fix", nil, &sum, 100); err != nil {
		t.Fatalf("insert round 2: %v", err)
	}

	info := stepToInfo(d, step)
	if len(info.FixSummaries) != 1 || info.FixSummaries[0] != sum {
		t.Errorf("fix summaries = %v, want [%q]", info.FixSummaries, sum)
	}
}

func TestStepToInfoNoFixSummariesWithoutFixRounds(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepLint)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if _, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 100); err != nil {
		t.Fatalf("insert round: %v", err)
	}

	info := stepToInfo(d, step)
	if len(info.FixSummaries) != 0 {
		t.Errorf("fix summaries = %v, want none", info.FixSummaries)
	}
}

func TestRunToInfoCarriesNonconvergenceSignalOrLeavesItAbsent(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// Not measured: the field must be absent, never a default value.
	if info := runToInfo(d, run, nil); info.Nonconvergence != nil {
		t.Fatalf("unmeasured run must carry no signal, got %+v", info.Nonconvergence)
	}

	themes := 1.25
	if err := d.RecordRunNonconvergence(run.ID, db.Nonconvergence{
		Probability: 0.73,
		Model:       "typesafe/jev-1.13-20260917",
		Step:        string(types.StepReview),
		Round:       3,
		Themes:      &themes,
		ObservedAt:  1700000000,
	}); err != nil {
		t.Fatalf("record signal: %v", err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}

	info := runToInfo(d, run, nil)
	if info.Nonconvergence == nil {
		t.Fatal("expected the signal on the wire")
	}
	if info.Nonconvergence.Probability != 0.73 {
		t.Errorf("probability = %v, want 0.73", info.Nonconvergence.Probability)
	}
	if info.Nonconvergence.Model != "typesafe/jev-1.13-20260917" {
		t.Errorf("model = %q, want the responding model version", info.Nonconvergence.Model)
	}
	if info.Nonconvergence.Round != 3 || info.Nonconvergence.Step != string(types.StepReview) {
		t.Errorf("round/step = %d/%q, want 3/review", info.Nonconvergence.Round, info.Nonconvergence.Step)
	}
	if info.Nonconvergence.CausalThemes == nil || *info.Nonconvergence.CausalThemes != 1.25 {
		t.Errorf("causal themes = %v, want 1.25", info.Nonconvergence.CausalThemes)
	}
	if info.Nonconvergence.ObservedAt != 1700000000 {
		t.Errorf("observed_at = %d", info.Nonconvergence.ObservedAt)
	}
}
