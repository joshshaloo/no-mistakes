package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLegacyCustodyMigrationPreservesButNeverPromotesCrashEvidence(t *testing.T) {
	for _, shape := range []string{"exact", "packed", "mismatched", "active", "symbolic", "conflicting_recovery"} {
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
			defer d.Close()
			repo, base := setupTestGitRepo(t, p, d, "legacy")
			run, err := d.InsertRun(repo.ID, "feature", base, base)
			if err != nil {
				t.Fatal(err)
			}
			gate, wd := p.RepoDir(repo.ID), p.WorktreeDir(repo.ID, run.ID)
			gr := func(dir string, args ...string) string {
				t.Helper()
				out, err := git.Run(ctx, dir, args...)
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			gr(gate, "update-ref", "refs/heads/feature", base)
			if err := git.WorktreeAdd(ctx, gate, wd, base); err != nil {
				t.Fatal(err)
			}
			commit := func(name string) string {
				t.Helper()
				if err := os.WriteFile(filepath.Join(wd, name), []byte(name), 0o644); err != nil {
					t.Fatal(err)
				}
				gr(wd, "add", name)
				gr(wd, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", name)
				return gr(wd, "rev-parse", "HEAD")
			}
			recorded := commit("recorded")
			if err := d.UpdateRunHeadSHA(run.ID, recorded); err != nil {
				t.Fatal(err)
			}
			if err := git.PinRunHead(ctx, gate, run.ID, recorded); err != nil {
				t.Fatal(err)
			}
			crash := commit("unrecorded")
			if err := git.PinExactCommit(ctx, gate, git.CrashHeadRef(run.ID), crash); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			switch shape {
			case "packed":
				gr(gate, "pack-refs", "--all", "--prune")
			case "mismatched":
				if err := git.PinRunHead(ctx, gate, run.ID, crash); err != nil {
					t.Fatal(err)
				}
			case "active":
				if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
			case "symbolic":
				gr(gate, "symbolic-ref", git.RunHeadRef(run.ID), "refs/heads/feature")
			case "conflicting_recovery":
				if err := custody.PreserveRecoveryHead(ctx, gate, run.ID, crash); err != nil {
					t.Fatal(err)
				}
			}
			if err := git.WorktreeRemove(ctx, gate, wd); err != nil {
				t.Fatal(err)
			}
			err = migrateLegacyPreservedHeads(ctx, d, p)
			if shape == "symbolic" || shape == "conflicting_recovery" {
				if err == nil {
					t.Fatal("ambiguous authority accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			stored, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantVerified := shape == "exact" || shape == "packed"
			if stored.HeadSHA != recorded || (stored.TerminalHeadVerifiedAt != nil) != wantVerified {
				t.Fatalf("migration promoted/erased authority: %+v", stored)
			}
			if gr(gate, "rev-parse", git.CrashHeadRef(run.ID)) != crash {
				t.Fatal("crash evidence changed")
			}
			if !wantVerified {
				return
			}
			if got, err := resolveRerunHead(ctx, gate, "feature", stored); err != nil || got != recorded {
				t.Fatalf("legacy rerun head=%s err=%v", got, err)
			}
			if err := migrateLegacyPreservedHeads(ctx, d, p); err != nil {
				t.Fatal(err)
			}
			gr(gate, "reflog", "expire", "--expire=now", "--all")
			gr(gate, "gc", "--prune=now")
			for _, head := range []string{recorded, crash} {
				if got := gr(gate, "rev-parse", git.ArchiveHeadRef(run.ID, head)+"^{commit}"); got != head {
					t.Fatalf("lost archived head %s", head)
				}
			}
			if gr(gate, "rev-parse", custody.RecoveryRef(run.ID)) != recorded {
				t.Fatal("crash evidence promoted to recovery authority")
			}
		})
	}
}
