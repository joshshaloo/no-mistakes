package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The upstream custody model may release a third head; it may not make that
// head unreachable. Test real objects after reflog expiry and aggressive GC.
func TestCleanupArchivesUnlandedHeadsBeforeRemovingWorktree(t *testing.T) {
	for _, shape := range []string{"descendant", "sibling", "legacy_candidate"} {
		t.Run(shape, func(t *testing.T) {
			ctx := context.Background()
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			repo, base := setupTestGitRepo(t, p, d, "archive-cleanup")
			run, err := d.InsertRun(repo.ID, "feature", base, base)
			if err != nil {
				t.Fatal(err)
			}
			wd, gate := p.WorktreeDir(repo.ID, run.ID), p.RepoDir(repo.ID)
			if err := git.WorktreeAdd(ctx, gate, wd, base); err != nil {
				t.Fatal(err)
			}
			gitRun := func(dir string, args ...string) string {
				t.Helper()
				out, err := git.Run(ctx, dir, args...)
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			commit := func(name string) string {
				t.Helper()
				if err := os.WriteFile(filepath.Join(wd, name), []byte(name+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitRun(wd, "add", name)
				gitRun(wd, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", name)
				return gitRun(wd, "rev-parse", "HEAD")
			}
			recorded := commit("recorded.txt")
			if err := d.UpdateRunHeadSHA(run.ID, recorded); err != nil {
				t.Fatal(err)
			}
			if shape != "descendant" {
				gitRun(wd, "checkout", "--detach", base)
			}
			candidate := commit("candidate.txt")
			if shape == "legacy_candidate" {
				if err := git.PinRunHead(ctx, gate, run.ID, candidate); err != nil {
					t.Fatal(err)
				}
				gitRun(wd, "checkout", "--detach", base)
			}
			live := commit("live.txt")
			if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			if err := cleanupRunWorktree(ctx, d, gate, wd, run.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(wd); !os.IsNotExist(err) {
				t.Fatalf("worktree remains: %v", err)
			}
			gitRun(gate, "reflog", "expire", "--expire=now", "--all")
			gitRun(gate, "gc", "--prune=now")
			for _, head := range []string{recorded, live} {
				if got := gitRun(gate, "rev-parse", git.ArchiveHeadRef(run.ID, head)+"^{commit}"); got != head {
					t.Fatalf("archive=%s want %s", got, head)
				}
			}
			if shape == "legacy_candidate" && gitRun(gate, "rev-parse", git.RunHeadRef(run.ID)) != candidate {
				t.Fatal("legacy candidate lost")
			}
			stored, err := d.GetRun(run.ID)
			if err != nil || stored.HeadSHA != recorded {
				t.Fatalf("cleanup promoted authority: %+v %v", stored, err)
			}
		})
	}
}
