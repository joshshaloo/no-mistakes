package branchsync

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRecoverKeepLocalRefusesMissingUnarchivableHead(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	if err := f.db.UpdateRunHeadSHA(f.run.ID, strings.Repeat("f", 40)); err != nil {
		t.Fatal(err)
	}
	before := mustRun(t, f.gate, "for-each-ref", "--format=%(refname) %(objectname)")
	state := f.service.Recover(f.ctx, true)
	if state.Recovered || f.custodyReturned() {
		t.Fatalf("returned custody without an archive of missing work: %#v", state)
	}
	if after := mustRun(t, f.gate, "for-each-ref", "--format=%(refname) %(objectname)"); after != before {
		t.Fatal("refusal mutated gate refs")
	}
}

func TestRecoverKeepLocalArchivesThirdHeadThroughGC(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	tree := mustRun(t, f.gate, "rev-parse", f.preserved+"^{tree}")
	third := mustRun(t, f.gate, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", tree, "-p", f.submitted, "-m", "independent gate work")
	mustRun(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), f.preserved)
	mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", third)
	state := f.service.Recover(f.ctx, true)
	if !state.Recovered {
		t.Fatalf("third-head recovery refused: %#v", state)
	}
	mustRun(t, f.gate, "reflog", "expire", "--expire=now", "--all")
	mustRun(t, f.gate, "gc", "--prune=now")
	for ref, head := range map[string]string{custody.RecoveryRef(f.run.ID): f.preserved, custody.RecoveryGateRef(f.run.ID): third} {
		if got := mustRun(t, f.gate, "rev-parse", ref+"^{commit}"); got != head {
			t.Fatalf("%s = %s, want %s", ref, got, head)
		}
	}
}

func TestRecoverMissingBranchUsesReachableArchive(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.gate, "update-ref", custody.RecoveryRef(f.run.ID), f.preserved)
	mustRun(t, f.gate, "update-ref", "-d", "refs/heads/feature/recover")
	state := f.service.Recover(f.ctx, true)
	if !state.Recovered {
		t.Fatalf("archived recovery refused: %#v", state)
	}
	mustRun(t, f.gate, "reflog", "expire", "--expire=now", "--all")
	mustRun(t, f.gate, "gc", "--prune=now")
	if got := mustRun(t, f.gate, "rev-parse", custody.RecoveryRef(f.run.ID)+"^{commit}"); got != f.preserved {
		t.Fatalf("archive = %s", got)
	}
}
