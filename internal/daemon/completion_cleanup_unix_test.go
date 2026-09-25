//go:build unix

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type unrecordedHeadStep struct{}

func (*unrecordedHeadStep) Name() types.StepName { return types.StepLint }
func (*unrecordedHeadStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, "unrecorded.txt"), []byte("preserve this work\n"), 0o644); err != nil {
		return nil, err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "add", "unrecorded.txt"); err != nil {
		return nil, err
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "unrecorded head"); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}

func TestRunCompletionFailsWhenCleanupRetainsWorktree(t *testing.T) {
	p, database, repo, head := newRunCleanupFixture(t)
	mgr := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{&unrecordedHeadStep{}} })
	runID, err := mgr.startRun(context.Background(), repo, "target", head, head, "test", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	waitForRunDone(t, mgr, runID)
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, errWorktreeRetainedForCustody.Error()) {
		t.Fatalf("retained run = %+v, want failed with custody diagnostic", run)
	}
	workDir := p.WorktreeDir(repo.ID, runID)
	if data, err := os.ReadFile(filepath.Join(workDir, "unrecorded.txt")); err != nil || string(data) != "preserve this work\n" {
		t.Fatalf("retained work = %q, %v", data, err)
	}
	if pinned, err := git.ResolveRef(context.Background(), p.RepoDir(repo.ID), git.RunHeadRef(runID)); err != nil || pinned != head {
		t.Fatalf("recorded head = %q, %v", pinned, err)
	}
	if crash, err := git.ResolveRef(context.Background(), p.RepoDir(repo.ID), git.CrashHeadRef(runID)); err != nil || crash == head || crash == "" {
		t.Fatalf("crash head = %q, %v", crash, err)
	}
}
