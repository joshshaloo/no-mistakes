package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewRisk_MissingRunDatabaseFailsClosed(t *testing.T) {
	if _, err := RefreshReviewRisk(context.Background(), nil, "owned-run", t.TempDir(), "", nil); !errors.Is(err, ErrReviewRisk) {
		t.Fatalf("refresh without a run database = %v", err)
	}
	if _, err := StoredReviewRiskStale(nil, "owned-run"); !errors.Is(err, ErrReviewRisk) {
		t.Fatalf("read without a run database = %v", err)
	}
}

func TestOnlyMechanicalDocumentation(t *testing.T) {
	for _, tt := range []struct {
		name, raw string
		want      bool
	}{
		{"empty commit", "", true},
		{"docs edit", ":100644 100644 aaa bbb M\x00docs/guide.md\x00", true},
		{"root addition", ":000000 100644 000 bbb A\x00CONTRIBUTING.md\x00", true},
		{"docs deletion", ":100644 000000 aaa 000 D\x00docs/old.rst\x00", true},
		{"docs symlink", ":000000 120000 000 bbb A\x00docs/guide.md\x00", false},
		{"executable bit", ":100644 100755 aaa aaa M\x00docs/guide.md\x00", false},
		{"type change", ":100644 120000 aaa bbb T\x00docs/guide.md\x00", false},
		{"mixed", ":100644 100644 aaa bbb M\x00docs/guide.md\x00:000000 100644 000 bbb A\x00main.go\x00", false},
		{"rename not filtered", ":100644 100644 aaa aaa R100\x00docs/a.md\x00src/a.md\x00", false},
		{"nested guidance", ":100644 100644 aaa bbb M\x00docs/AGENTS.md\x00", false},
		{"path contains newline", ":100644 100644 aaa bbb M\x00bin/teardown\nREADME.md\x00", false},
		{"malformed", "oops", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := onlyMechanicalDocumentation(tt.raw); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRefreshReviewRisk_SubmoduleChangeOverridesIgnoreConfiguration(t *testing.T) {
	database, _, run, _ := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	submoduleDir := filepath.Join(dir, "vendor", "dependency")
	if err := os.MkdirAll(submoduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, submoduleDir)
	execGit(t, dir, "add", "vendor/dependency")
	execGit(t, dir, "commit", "-m", "add dependency")
	base, err := git.Run(context.Background(), dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	base = strings.TrimSpace(base)
	if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, base); err != nil {
		t.Fatal(err)
	}
	sr, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"findings":[],"risk_level":"low","risk_rationale":"old"}`
	if err := database.SetStepFindings(sr.ID, raw); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(sr.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, submoduleDir, "README.md", "# changed dependency\n")
	execGit(t, submoduleDir, "add", "README.md")
	execGit(t, submoduleDir, "commit", "-m", "change dependency")
	execGit(t, dir, "add", "vendor/dependency")
	execGit(t, dir, "config", "diff.ignoreSubmodules", "all")

	stale, err := RefreshReviewRisk(context.Background(), database, run.ID, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("gitlink change hidden by diff.ignoreSubmodules retained the prior risk")
	}
}

func TestRefreshReviewRisk_LegacyFallbackRetiredWithoutRewritingHistory(t *testing.T) {
	database, _, run, _ := setupTest(t)
	sr, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"findings":[],"risk_level":"low","risk_rationale":"old"}`
	if _, err := database.InsertStepRound(sr.ID, 1, "initial", &raw, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(sr.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		stale, err := RefreshReviewRisk(context.Background(), database, run.ID, t.TempDir(), "", nil)
		if err != nil || !stale {
			t.Fatalf("stale=%v, err=%v", stale, err)
		}
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*steps[0].FindingsJSON, `"risk_level":"stale"`) {
		t.Fatal("legacy round fallback still current")
	}
	rounds, err := database.GetRoundsByStep(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *rounds[0].FindingsJSON != raw {
		t.Fatal("history rewritten")
	}
}
