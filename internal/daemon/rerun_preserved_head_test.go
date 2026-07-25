package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type rerunHeadFixture struct {
	ctx       context.Context
	p         *paths.Paths
	database  *db.DB
	manager   *RunManager
	repo      *db.Repo
	run       *db.Run
	gate      string
	submitted string
	preserved string
}

func newRerunHeadFixture(t *testing.T) *rerunHeadFixture {
	t.Helper()
	ctx := context.Background()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, submitted := setupTestGitRepo(t, p, database, "rerun-head")
	gate := p.RepoDir(repo.ID)

	worktree := filepath.Join(t.TempDir(), "pipeline")
	if err := git.WorktreeAdd(ctx, gate, worktree, submitted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = git.WorktreeRemove(ctx, gate, worktree) })
	gitCmd(t, worktree, "config", "user.name", "test")
	gitCmd(t, worktree, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(worktree, "pipeline.txt"), []byte("preserved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, worktree, "add", "pipeline.txt")
	gitCmd(t, worktree, "commit", "-m", "pipeline head")
	preserved := gitOutput(t, worktree, "rev-parse", "HEAD")

	run, err := database.InsertRun(repo.ID, "main", submitted, submitted)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)
	return &rerunHeadFixture{ctx: ctx, p: p, database: database, manager: NewRunManager(database, p, nil), repo: repo, run: run, gate: gate, submitted: submitted, preserved: preserved}
}

func TestResolveRerunHeadRefusesStaleMutableBranchWithoutExactRunRef(t *testing.T) {
	f := newRerunHeadFixture(t)
	runID, err := f.manager.HandleRerun(f.ctx, f.repo.ID, "main", nil, "")
	if err == nil || !strings.Contains(err.Error(), "recover custody") {
		t.Fatalf("rerun = %s, %v; want custody refusal", runID, err)
	}
	if runID != "" {
		t.Fatalf("rerun selected stale mutable head in new run %s", runID)
	}
	if got := gitOutput(t, f.gate, "rev-parse", "refs/heads/main"); got != f.submitted {
		t.Fatal("rerun refusal mutated gate branch")
	}
	runs, _ := f.database.GetRunsByRepo(f.repo.ID)
	if len(runs) != 1 {
		t.Fatalf("rerun refusal created another run: %d", len(runs))
	}
}

func TestResolveRerunHeadDoesNotBuryOlderUnresolvedCustody(t *testing.T) {
	f := newRerunHeadFixture(t)
	newer, err := f.database.InsertRun(f.repo.ID, "main", f.submitted, f.submitted)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.database.UpdateRunStatus(newer.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.manager.resolveRerunHead(f.ctx, f.repo, "main"); err == nil || !strings.Contains(err.Error(), f.run.ID) {
		t.Fatalf("rerun did not retain older unresolved owner %s: %v", f.run.ID, err)
	}
}

func TestResolveRerunHeadUsesExactVerifiedRunOwnedHead(t *testing.T) {
	f := newRerunHeadFixture(t)
	if err := git.PinRunHead(f.ctx, f.gate, f.run.ID, f.preserved); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(f.ctx, f.gate, "update-ref", "refs/heads/main", f.preserved, f.submitted); err != nil {
		t.Fatal(err)
	}
	head, base, err := f.manager.resolveRerunHead(f.ctx, f.repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if head != f.preserved || base != f.run.BaseSHA {
		t.Fatalf("rerun resolved head/base = %s/%s, want %s/%s", head, base, f.preserved, f.run.BaseSHA)
	}
}

func TestResolveRerunHeadRefusesPublishedRefBeforeFailedDatabaseWriteIsCleanedUp(t *testing.T) {
	f := newRerunHeadFixture(t)
	if err := f.database.UpdateRunHeadSHA(f.run.ID, f.submitted); err != nil {
		t.Fatal(err)
	}
	f.run.HeadSHA = f.submitted
	if err := git.PinRunHead(f.ctx, f.gate, f.run.ID, f.preserved); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(f.ctx, f.gate, "update-ref", "refs/heads/main", f.preserved, f.submitted); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.manager.resolveRerunHead(f.ctx, f.repo, "main"); err == nil || !strings.Contains(err.Error(), "recover custody") {
		t.Fatalf("submitted-authority ambiguous rerun error = %v", err)
	}
}

func TestResolveRerunHeadRefusesAmbiguousCrashCandidate(t *testing.T) {
	f := newRerunHeadFixture(t)
	if err := git.PinRunHead(f.ctx, f.gate, f.run.ID, f.preserved); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(f.ctx, f.gate, "update-ref", "refs/heads/main", f.preserved, f.submitted); err != nil {
		t.Fatal(err)
	}
	if err := git.PinExactCommit(f.ctx, f.gate, git.CrashHeadRef(f.run.ID), f.submitted); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.manager.resolveRerunHead(f.ctx, f.repo, "main"); err == nil || !strings.Contains(err.Error(), "recover custody") {
		t.Fatalf("ambiguous rerun error = %v", err)
	}
}
