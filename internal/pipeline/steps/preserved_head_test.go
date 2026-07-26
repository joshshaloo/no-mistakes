package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

type rebasePublicationFixture struct {
	ctx      context.Context
	gate     string
	worktree string
	upstream string
	oldHead  string
	base     string
	database *db.DB
	sctx     *pipeline.StepContext
}

func newRebasePublicationFixture(t *testing.T) *rebasePublicationFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	gitCmd(t, root, "init", "--bare", upstream)

	seed := filepath.Join(root, "seed")
	gitCmd(t, root, "init", "-b", "main", seed)
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "base.txt")
	gitCmd(t, seed, "commit", "-m", "base")
	base := gitCmd(t, seed, "rev-parse", "HEAD")
	gitCmd(t, seed, "remote", "add", "origin", upstream)
	gitCmd(t, seed, "push", "origin", "main")

	gitCmd(t, seed, "checkout", "-b", "feature/recover")
	if err := os.WriteFile(filepath.Join(seed, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "feature.txt")
	gitCmd(t, seed, "commit", "-m", "feature")
	oldHead := gitCmd(t, seed, "rev-parse", "HEAD")

	gate := filepath.Join(root, "gate.git")
	gitCmd(t, root, "init", "--bare", gate)
	gitCmd(t, seed, "push", gate, "HEAD:refs/heads/feature/recover")
	gitCmd(t, gate, "remote", "add", "origin", upstream)

	gitCmd(t, seed, "checkout", "main")
	if err := os.WriteFile(filepath.Join(seed, "main.txt"), []byte("advanced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "main.txt")
	gitCmd(t, seed, "commit", "-m", "advance main")
	gitCmd(t, seed, "push", "origin", "main")

	worktree := filepath.Join(root, "pipeline")
	if err := git.WorktreeAdd(ctx, gate, worktree, oldHead); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = git.WorktreeRemove(ctx, gate, worktree) })
	gitCmd(t, worktree, "config", "user.name", "test")
	gitCmd(t, worktree, "config", "user.email", "test@example.com")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(seed, upstream, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", oldHead, base)
	if err != nil {
		t.Fatal(err)
	}
	sctx := &pipeline.StepContext{
		Ctx: ctx, Run: run, Repo: repo, WorkDir: worktree, DB: database,
		Agent: &mockAgent{name: "test"}, Config: &config.Config{},
		Log: func(string) {}, LogChunk: func(string) {}, LogFile: func(string) {},
	}
	return &rebasePublicationFixture{ctx: ctx, gate: gate, worktree: worktree, upstream: upstream, oldHead: oldHead, base: base, database: database, sctx: sctx}
}

func TestRebaseStepPublishesExactHeadBeforeRunAuthority(t *testing.T) {
	f := newRebasePublicationFixture(t)
	if _, err := (&RebaseStep{}).Execute(f.sctx); err != nil {
		t.Fatal(err)
	}
	newHead := gitCmd(t, f.worktree, "rev-parse", "HEAD")
	if newHead == f.oldHead {
		t.Fatal("rebase did not advance the detached worktree")
	}
	for label, got := range map[string]string{
		"gate branch": gitCmd(t, f.gate, "rev-parse", "refs/heads/feature/recover"),
		"run ref":     gitCmd(t, f.gate, "rev-parse", git.RunHeadRef(f.sctx.Run.ID)),
		"memory":      f.sctx.Run.HeadSHA,
	} {
		if got != newHead {
			t.Fatalf("%s = %s, want %s", label, got, newHead)
		}
	}
	reloaded, err := f.database.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.HeadSHA != newHead {
		t.Fatalf("database head = %s, want %s", reloaded.HeadSHA, newHead)
	}
}

func TestCommitAgentFixesPublishesRunOwnedHead(t *testing.T) {
	dir, base, oldHead := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, oldHead, config.Commands{})
	if err := os.WriteFile(filepath.Join(dir, "ordinary-fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAgentFixes(sctx, "review", "preserve ordinary fix", "fallback"); err != nil {
		t.Fatal(err)
	}
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if newHead == oldHead {
		t.Fatal("ordinary fix did not create a commit")
	}
	if got := gitCmd(t, dir, "rev-parse", git.RunHeadRef(sctx.Run.ID)); got != newHead {
		t.Fatalf("run head ref = %s, want %s", got, newHead)
	}
	reloaded, _ := sctx.DB.GetRun(sctx.Run.ID)
	if reloaded.HeadSHA != newHead || sctx.Run.HeadSHA != newHead {
		t.Fatalf("ordinary fix authority memory/db = %s/%s, want %s", sctx.Run.HeadSHA, reloaded.HeadSHA, newHead)
	}
}

func TestPublishPipelineHeadBranchCASPreservesRaceWinner(t *testing.T) {
	f := newRebasePublicationFixture(t)
	if err := git.FetchRemoteBranch(f.ctx, f.worktree, "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(f.ctx, f.worktree, "rebase", "origin/main"); err != nil {
		t.Fatal(err)
	}
	newHead := gitCmd(t, f.worktree, "rev-parse", "HEAD")

	writer := filepath.Join(t.TempDir(), "writer")
	gitCmd(t, filepath.Dir(writer), "clone", f.gate, writer)
	gitCmd(t, writer, "config", "user.name", "test")
	gitCmd(t, writer, "config", "user.email", "test@example.com")
	gitCmd(t, writer, "checkout", "feature/recover")
	if err := os.WriteFile(filepath.Join(writer, "race.txt"), []byte("winner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, writer, "add", "race.txt")
	gitCmd(t, writer, "commit", "-m", "race winner")
	winner := gitCmd(t, writer, "rev-parse", "HEAD")
	gitCmd(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")

	err := publishPipelineHead(f.sctx, newHead)
	if err == nil || !strings.Contains(err.Error(), "compare-and-swap") {
		t.Fatalf("publish error = %v, want compare-and-swap refusal", err)
	}
	if got := gitCmd(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != winner {
		t.Fatalf("race winner clobbered: got %s want %s", got, winner)
	}
	if got := gitCmd(t, f.gate, "rev-parse", git.RunHeadRef(f.sctx.Run.ID)); got != newHead {
		t.Fatalf("new pipeline head was not preserved: got %s want %s", got, newHead)
	}
	if f.sctx.Run.HeadSHA != f.oldHead {
		t.Fatalf("in-memory authority advanced to %s", f.sctx.Run.HeadSHA)
	}
	reloaded, _ := f.database.GetRun(f.sctx.Run.ID)
	if reloaded.HeadSHA != f.oldHead {
		t.Fatalf("database authority advanced to %s", reloaded.HeadSHA)
	}
}

func TestPublishPipelineHeadDatabaseFailureRetainsExactGitAnchor(t *testing.T) {
	f := newRebasePublicationFixture(t)
	if err := git.FetchRemoteBranch(f.ctx, f.worktree, "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(f.ctx, f.worktree, "rebase", "origin/main"); err != nil {
		t.Fatal(err)
	}
	newHead := gitCmd(t, f.worktree, "rev-parse", "HEAD")
	if err := f.database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := publishPipelineHead(f.sctx, newHead); err == nil || !strings.Contains(err.Error(), "preserved at") {
		t.Fatalf("publish with closed DB error = %v", err)
	}
	if got := gitCmd(t, f.gate, "rev-parse", git.RunHeadRef(f.sctx.Run.ID)); got != newHead {
		t.Fatalf("database failure lost exact run anchor: %s", got)
	}
	if got := gitCmd(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != newHead {
		t.Fatalf("database failure lost published branch: %s", got)
	}
	if f.sctx.Run.HeadSHA != f.oldHead {
		t.Fatalf("in-memory authority advanced despite DB failure: %s", f.sctx.Run.HeadSHA)
	}
}
