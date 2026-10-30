package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// materializeCleanupWorktree gives placement-only fixtures real custody
// evidence. Removal now requires an archive, so a directory plus a made-up SHA
// must not be used to model a removable terminal worktree.
func materializeCleanupWorktree(t *testing.T, d *db.DB, p *paths.Paths, run *db.Run, dir string) {
	t.Helper()
	ctx := context.Background()
	gateDir := p.RepoDir(run.RepoID)
	if _, err := os.Stat(filepath.Join(gateDir, "HEAD")); os.IsNotExist(err) {
		if _, err := git.Run(ctx, t.TempDir(), "init", "--bare", gateDir); err != nil {
			t.Fatal(err)
		}
	}
	tree, err := git.Run(ctx, gateDir, "mktree")
	if err != nil {
		t.Fatal(err)
	}
	head, err := git.Run(ctx, gateDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", tree, "-m", "cleanup fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	if err := git.WorktreeAdd(ctx, gateDir, dir, head); err != nil {
		t.Fatal(err)
	}
}
