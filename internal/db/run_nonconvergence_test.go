package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func newNonconvergenceRun(t *testing.T) (*DB, *Run) {
	t.Helper()
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo-nonconv", "https://example.com/r.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	return d, run
}

func TestRunNonconvergenceIsAbsentUntilRecorded(t *testing.T) {
	d, run := newNonconvergenceRun(t)

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.NonconvergenceProbability != nil {
		t.Fatalf("a fresh run must carry no measurement, got %v", *got.NonconvergenceProbability)
	}
	if got.Nonconvergence() != nil {
		t.Fatal("Nonconvergence() must be nil when nothing was measured; nil is 'not measured', never 'converging'")
	}
}

func TestRecordRunNonconvergenceStoresProbabilityAndModelVersion(t *testing.T) {
	d, run := newNonconvergenceRun(t)
	themes := 1.4

	if err := d.RecordRunNonconvergence(run.ID, Nonconvergence{
		Probability: 0.82,
		Model:       "typesafe/jev-1.13-20260917",
		Step:        "review",
		Round:       3,
		Themes:      &themes,
		ObservedAt:  1700000000,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	nc := got.Nonconvergence()
	if nc == nil {
		t.Fatal("expected a recorded measurement")
	}
	if nc.Probability != 0.82 {
		t.Errorf("probability = %v, want 0.82 (the probability is stored, not a boolean)", nc.Probability)
	}
	if nc.Model != "typesafe/jev-1.13-20260917" {
		t.Errorf("model = %q, want the exact responding model version", nc.Model)
	}
	if nc.Step != "review" || nc.Round != 3 {
		t.Errorf("step/round = %q/%d, want review/3", nc.Step, nc.Round)
	}
	if nc.Themes == nil || *nc.Themes != 1.4 {
		t.Errorf("themes = %v, want 1.4", nc.Themes)
	}
	if nc.ObservedAt != 1700000000 {
		t.Errorf("observed_at = %d", nc.ObservedAt)
	}
}

func TestRecordRunNonconvergenceReplacesTheEarlierMeasurement(t *testing.T) {
	d, run := newNonconvergenceRun(t)

	if err := d.RecordRunNonconvergence(run.ID, Nonconvergence{Probability: 0.2, Model: "m", Step: "review", Round: 2, ObservedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRunNonconvergence(run.ID, Nonconvergence{Probability: 0.9, Model: "m2", Step: "review", Round: 3, ObservedAt: 2}); err != nil {
		t.Fatal(err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	nc := got.Nonconvergence()
	if nc.Probability != 0.9 || nc.Round != 3 || nc.Model != "m2" {
		t.Fatalf("latest measurement must win, got %+v", nc)
	}
}

func TestRecordRunNonconvergenceTouchesNoRunOutcome(t *testing.T) {
	d, run := newNonconvergenceRun(t)
	before, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := d.RecordRunNonconvergence(run.ID, Nonconvergence{Probability: 0.99, Model: "m", Step: "review", Round: 4, ObservedAt: 7}); err != nil {
		t.Fatal(err)
	}

	after, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != before.Status {
		t.Errorf("status changed: %q -> %q; the signal must never change an outcome", before.Status, after.Status)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("updated_at changed: %d -> %d; recording a signal is not run progress", before.UpdatedAt, after.UpdatedAt)
	}
	if after.HeadSHA != before.HeadSHA || after.Error != before.Error {
		t.Error("the signal write must not touch head or error")
	}
	if after.AwaitingAgentSince != before.AwaitingAgentSince {
		t.Error("the signal write must not touch the parked marker")
	}
}

func TestRecordRunNonconvergenceOnUnknownRunIsANoOp(t *testing.T) {
	d, _ := newNonconvergenceRun(t)
	if err := d.RecordRunNonconvergence("no-such-run", Nonconvergence{Probability: 0.5, Model: "m", Step: "review", Round: 2, ObservedAt: 1}); err != nil {
		t.Fatalf("recording against an unknown run must not error: %v", err)
	}
}

// A database created before the signal existed must gain the columns and read
// back as "not measured", never as a fabricated zero probability.
func TestOpenMigratesLegacyRunsToNonconvergenceColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")

	legacyDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE runs (
			id TEXT PRIMARY KEY,
			repo_id TEXT NOT NULL,
			branch TEXT NOT NULL,
			head_sha TEXT NOT NULL,
			base_sha TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			pr_url TEXT,
			error TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, created_at, updated_at)
		VALUES ('legacy-run', 'repo', 'feature', 'abc', 'def', 'completed', 1, 1);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy runs table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	got, err := d.GetRun("legacy-run")
	if err != nil {
		t.Fatalf("get legacy run: %v", err)
	}
	if got == nil {
		t.Fatal("expected the legacy run to survive migration")
	}
	if got.Nonconvergence() != nil {
		t.Fatal("a legacy run must read back as not measured, never as a fabricated value")
	}
}
