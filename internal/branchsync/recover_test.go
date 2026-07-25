package branchsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// recoverFixture reproduces the stranded custody state from the v1.38.1
// dogfood report: a run went terminal at the pre_push phase, so its pipeline
// fix commits exist only in the local gate's bare branch while the registered
// operator worktree still sits at the submitted head with no push binding.
type recoverFixture struct {
	t         *testing.T
	ctx       context.Context
	db        *db.DB
	repo      *db.Repo
	run       *db.Run
	service   *Service
	local     string
	gate      string
	pipeline  string
	remote    string
	base      string
	submitted string
	preserved string
}

// newRecoverFixture builds an operator repo on feature/recover at the
// submitted head, a bare gate whose feature/recover branch carries two extra
// pipeline fix commits (the preserved head), and a run row that is terminal
// with head_sha at the preserved head and no push provenance.
func newRecoverFixture(t *testing.T, status types.RunStatus) *recoverFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "checkout", "-b", "feature/recover")
	mustWrite(t, filepath.Join(local, "file.txt"), "feature\n")
	mustRun(t, local, "commit", "-am", "feature")
	submitted := mustRun(t, local, "rev-parse", "HEAD")

	// The gate receives the submitted branch, then the pipeline commits fixes
	// onto the gate branch; nothing is ever pushed to the upstream.
	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/feature/recover:refs/heads/feature/recover")
	pipeline := filepath.Join(root, "pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(pipeline, "fix.txt"), "pipeline fix\n")
	mustRun(t, pipeline, "add", "fix.txt")
	mustRun(t, pipeline, "commit", "-m", "no-mistakes(review): fix")
	mustWrite(t, filepath.Join(pipeline, "fix.txt"), "pipeline fix 2\n")
	mustRun(t, pipeline, "commit", "-am", "no-mistakes(lint): fix")
	preserved := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "origin", "HEAD:refs/heads/feature/recover")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, status); err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)
	return &recoverFixture{
		t: t, ctx: ctx, db: database, repo: repo, run: run,
		service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
		local:   local, gate: gate, pipeline: pipeline, remote: remote,
		base: base, submitted: submitted, preserved: preserved,
	}
}

func (f *recoverFixture) anchorRef() string { return "refs/no-mistakes/recover/" + f.run.ID }

func (f *recoverFixture) runHeadRef() string { return git.RunHeadRef(f.run.ID) }

func makeLegacyRebaseMismatch(t *testing.T, f *recoverFixture) {
	t.Helper()
	// Model a detached rebase R rather than the fixture's ordinary descendant
	// fix commit: preserve the same final tree on the immutable base, stage the
	// object into the gate, then remove every ref that names it.
	tree := mustRun(t, f.pipeline, "rev-parse", f.preserved+"^{tree}")
	rebased := mustRun(t, f.pipeline, "commit-tree", tree, "-p", f.base, "-m", "historical detached rebase")
	mustRun(t, f.pipeline, "push", f.gate, rebased+":refs/no-mistakes/legacy-stage")
	mustRun(t, f.gate, "update-ref", "-d", "refs/no-mistakes/legacy-stage")
	if err := f.db.UpdateRunHeadSHA(f.run.ID, rebased); err != nil {
		t.Fatal(err)
	}
	f.run.HeadSHA = rebased
	f.preserved = rebased
	mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted)
}

func (f *recoverFixture) custodyReturned() bool {
	f.t.Helper()
	run, err := f.db.GetRun(f.run.ID)
	if err != nil || run == nil {
		f.t.Fatalf("reload run: %#v, %v", run, err)
	}
	return run.CustodyReturnedAt != nil
}

// TestTerminalPrePushRunSurfacesGuardedCustodyRecovery is the regression test
// for the stranded state itself (dogfood run 01KXN8YJ6DWF8XPP582DWQC3HV): a
// terminal run at the pre_push phase must not be a dead end. The state stays
// pipeline_owned, but safety identifies it as recoverable, exposes the run's
// terminal status, and offers the guarded custody-return action.
func TestTerminalPrePushRunSurfacesGuardedCustodyRecovery(t *testing.T) {
	for _, status := range []types.RunStatus{types.RunCancelled, types.RunFailed, types.RunCompleted} {
		t.Run(string(status), func(t *testing.T) {
			f := newRecoverFixture(t, status)
			state := f.service.InspectCached(f.ctx)
			if state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned_recoverable" {
				t.Fatalf("state = %s safety = %s, want pipeline_owned/blocked_pipeline_owned_recoverable", state.State, state.Safety)
			}
			if state.Pipeline.Status != string(status) || state.Pipeline.Phase != "pre_push" {
				t.Fatalf("pipeline = %#v", state.Pipeline)
			}
			if state.NextAction == nil || state.NextAction.Code != "recover_custody" || !strings.Contains(state.NextAction.Command, "sync --recover") {
				t.Fatalf("next action = %#v", state.NextAction)
			}
			if !strings.Contains(state.Error, "preserved") {
				t.Fatalf("error does not explain preservation: %q", state.Error)
			}
		})
	}
}

// TestActivePrePushRunStaysBlockedWithoutRecovery pins the other half of the
// class split: while the run is still active the pre-push block is correct and
// no custody-return action may be offered.
func TestActivePrePushRunStaysBlockedWithoutRecovery(t *testing.T) {
	f := newRecoverFixture(t, types.RunRunning)
	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned" {
		t.Fatalf("active run state = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code != "continue_active_run" || state.NextAction.Command != "no-mistakes axi status" {
		t.Fatalf("active run next action = %#v", state.NextAction)
	}
	if state.Pipeline.Status != "running" {
		t.Fatalf("pipeline status = %q", state.Pipeline.Status)
	}
	recovered := f.service.Recover(f.ctx, false)
	if recovered.Recovered || recovered.Safety != "blocked_recover_run_active" {
		t.Fatalf("recover on active run = %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatal("recover on active run moved HEAD")
	}
	if f.custodyReturned() {
		t.Fatal("recover on active run stamped custody")
	}
}

// TestRecoverCleanBehindFastForwardsAndReturnsCustody is the primary recovery
// journey: terminal cancelled pre-push, clean worktree at the submitted head.
// Recovery must anchor the preserved commits locally, fast-forward the branch
// to the preserved head, stamp custody returned, and leave the branch free for
// a fresh run.
func TestRecoverCleanBehindFastForwardsAndReturnsCustody(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	state := f.service.Recover(f.ctx, false)
	if !state.Recovered || !state.Changed {
		t.Fatalf("recover result = %#v", state)
	}
	if state.State != StateCustodyReturned || state.Safety != "custody_returned" || state.Relation != RelationEqual {
		t.Fatalf("post-recover state = %s/%s relation %s", state.State, state.Safety, state.Relation)
	}
	if state.NextAction == nil || state.NextAction.Code != "run_pipeline" {
		t.Fatalf("post-recover next action = %#v", state.NextAction)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("HEAD = %s, want preserved %s", got, f.preserved)
	}
	if parents := strings.Fields(mustRun(t, f.local, "show", "-s", "--format=%P", f.preserved+"~1")); len(parents) != 1 || parents[0] != f.submitted {
		t.Fatalf("recovery rewrote history: %v", parents)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("anchor ref = %s, want %s", got, f.preserved)
	}
	if !f.custodyReturned() {
		t.Fatal("custody not stamped")
	}

	// The branch is free again: cached inspection reports custody_returned
	// with a run_pipeline next action, and a brand-new run takes over cleanly.
	after := f.service.InspectCached(f.ctx)
	if after.State != StateCustodyReturned || after.NextAction == nil || after.NextAction.Code != "run_pipeline" {
		t.Fatalf("post-recover inspection = %#v", after)
	}
	fresh, err := f.db.InsertRun(f.repo.ID, "feature/recover", f.preserved, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatus(fresh.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	next := f.service.InspectCached(f.ctx)
	if next.Pipeline.RunID != fresh.ID {
		t.Fatalf("fresh run not selected after recovery: %#v", next.Pipeline)
	}
}

func TestRecoverFastForwardRechecksCurrentBranchBeforeMerge(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	f.service.beforeRecoverFastForward = func() {
		mustRun(t, f.local, "checkout", "-b", "other-clean-branch", f.submitted)
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_assumptions_changed" {
		t.Fatalf("recover after branch switch = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("HEAD = %s, want submitted %s", got, f.submitted)
	}
	if got := strings.TrimSpace(mustRun(t, f.local, "branch", "--show-current")); got != "other-clean-branch" {
		t.Fatalf("current branch = %q", got)
	}
	if f.custodyReturned() {
		t.Fatal("branch-switch refusal stamped custody")
	}
}

func TestRecoverReportsDirtyFinalStateWhenPostMergeHookMutatesWorktree(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	hooks := filepath.Join(f.local, ".git", "hooks")
	hook := filepath.Join(hooks, "post-merge")
	mustWrite(t, hook, "#!/bin/sh\nprintf hook > hook-output.txt\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || !state.Changed || state.Local.Head != f.preserved || state.State != StateDirty || state.Local.Clean || !strings.HasPrefix(state.Safety, "blocked_post_recover_") {
		t.Fatalf("hook final state = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("honest final HEAD = %s", got)
	}
	if f.custodyReturned() {
		t.Fatal("dirty post-recover state stamped custody")
	}
}

// TestRecoverIdempotentAfterSuccess proves a repeated recover is a safe no-op.
func TestRecoverIdempotentAfterSuccess(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	if first := f.service.Recover(f.ctx, false); !first.Recovered {
		t.Fatalf("first recover = %#v", first)
	}
	second := f.service.Recover(f.ctx, false)
	if !second.Recovered || second.Changed || second.State != StateCustodyReturned {
		t.Fatalf("second recover = %#v", second)
	}
}

// TestRecoverWorktreeAlreadyAtPreservedHeadReturnsCustodyWithoutMutation
// covers the equal cell: nothing to reconcile, custody return only.
func TestRecoverWorktreeAlreadyAtPreservedHeadReturnsCustodyWithoutMutation(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.local, "fetch", f.gate, "refs/heads/feature/recover")
	mustRun(t, f.local, "merge", "--ff-only", f.preserved)
	if err := os.RemoveAll(f.gate); err != nil {
		t.Fatal(err)
	}
	state := f.service.Recover(f.ctx, false)
	if !state.Recovered || state.Changed || state.State != StateCustodyReturned || state.Relation != RelationEqual {
		t.Fatalf("recover equal = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("anchor ref = %s, want %s", got, f.preserved)
	}
	if !f.custodyReturned() {
		t.Fatal("custody not stamped")
	}
}

// TestRecoverLocalAheadOfPreservedHeadReturnsCustodyWithoutMutation covers the
// ahead cell: the preserved commits are already incorporated locally.
func TestRecoverLocalAheadOfPreservedHeadReturnsCustodyWithoutMutation(t *testing.T) {
	f := newRecoverFixture(t, types.RunFailed)
	mustRun(t, f.local, "fetch", f.gate, "refs/heads/feature/recover")
	mustRun(t, f.local, "merge", "--ff-only", f.preserved)
	mustWrite(t, filepath.Join(f.local, "followup.txt"), "followup\n")
	mustRun(t, f.local, "add", "followup.txt")
	mustRun(t, f.local, "commit", "-m", "followup")
	ahead := mustRun(t, f.local, "rev-parse", "HEAD")
	if err := os.RemoveAll(f.gate); err != nil {
		t.Fatal(err)
	}
	state := f.service.Recover(f.ctx, false)
	if !state.Recovered || state.Changed || state.State != StateCustodyReturned || state.Relation != RelationAhead {
		t.Fatalf("recover ahead = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != ahead {
		t.Fatal("recover ahead moved HEAD")
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("anchor ref = %s, want %s", got, f.preserved)
	}
}

// TestRecoverDirtyWorktreeRefusesWithoutMutation covers the behind+dirty cell:
// never fast-forward over uncommitted changes; refuse with actionable options.
func TestRecoverDirtyWorktreeRefusesWithoutMutation(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "file.txt"), "dirty\n")
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_dirty" {
		t.Fatalf("recover dirty = %#v", state)
	}
	if !strings.Contains(state.Error, "--keep-local") {
		t.Fatalf("dirty refusal not actionable: %q", state.Error)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatal("dirty refusal moved HEAD")
	}
	if f.custodyReturned() {
		t.Fatal("dirty refusal stamped custody")
	}
}

// TestRecoverDivergedRefusesButKeepLocalReturnsCustody covers the diverged
// cells: the default refuses with the anchor named, and --keep-local performs
// the explicit choice - custody at the local head, gate reset to it atomically,
// preserved commits still reachable through the anchor ref.
func TestRecoverDivergedRefusesButKeepLocalReturnsCustody(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "rescope.txt"), "rescope\n")
	mustRun(t, f.local, "add", "rescope.txt")
	mustRun(t, f.local, "commit", "-m", "diverging rescope")
	divergedHead := mustRun(t, f.local, "rev-parse", "HEAD")

	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || refused.Safety != "blocked_recover_diverged" || refused.Relation != RelationDiverged {
		t.Fatalf("recover diverged = %#v", refused)
	}
	if !strings.Contains(refused.Error, f.anchorRef()) || !strings.Contains(refused.Error, "--keep-local") {
		t.Fatalf("diverged refusal not actionable: %q", refused.Error)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("diverged refusal did not anchor preserved commits: %s", got)
	}
	if f.custodyReturned() {
		t.Fatal("diverged refusal stamped custody")
	}

	kept := f.service.Recover(f.ctx, true)
	if !kept.Recovered || kept.Changed {
		t.Fatalf("keep-local recover = %#v", kept)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != divergedHead {
		t.Fatal("keep-local moved the worktree")
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != divergedHead {
		t.Fatalf("gate branch = %s, want local head %s", got, divergedHead)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatal("keep-local lost the preserved anchor")
	}
	if !f.custodyReturned() {
		t.Fatal("keep-local did not stamp custody")
	}
}

// TestRecoverKeepLocalDirtyBehindReturnsCustodyWithoutTouchingWorktree covers
// the explicit keep-local choice on a dirty worktree: no worktree mutation is
// needed, so dirtiness must not block it, and the gate follows the kept head.
func TestRecoverKeepLocalDirtyBehindReturnsCustodyWithoutTouchingWorktree(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "file.txt"), "dirty rescope\n")
	state := f.service.Recover(f.ctx, true)
	if !state.Recovered || state.Changed {
		t.Fatalf("keep-local dirty recover = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatal("keep-local dirty moved HEAD")
	}
	if got := readOptional(t, filepath.Join(f.local, "file.txt")); got != "dirty rescope\n" {
		t.Fatal("keep-local dirty touched worktree files")
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("gate branch = %s, want kept head %s", got, f.submitted)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatal("keep-local dirty lost the preserved anchor")
	}
}

// TestRecoverGateDivergenceAndUnavailabilityFailClosed: recovery must refuse
// whenever the preserved head cannot be verified and anchored - a moved gate
// branch, a deleted gate branch, or a missing gate.
func TestRecoverGateDivergenceAndUnavailabilityFailClosed(t *testing.T) {
	t.Run("gate branch moved", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunCancelled)
		writer := filepath.Join(t.TempDir(), "writer")
		mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
		configureIdentity(t, writer)
		mustRun(t, writer, "checkout", "feature/recover")
		mustWrite(t, filepath.Join(writer, "other.txt"), "other\n")
		mustRun(t, writer, "add", "other.txt")
		mustRun(t, writer, "commit", "-m", "out of band gate commit")
		mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
		state := f.service.Recover(f.ctx, false)
		if state.Recovered || state.Safety != "blocked_recover_gate_diverged" {
			t.Fatalf("recover with moved gate = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
			t.Fatal("moved-gate refusal mutated HEAD")
		}
	})
	t.Run("gate branch deleted", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunCancelled)
		mustRun(t, f.gate, "update-ref", "-d", "refs/heads/feature/recover")
		state := f.service.Recover(f.ctx, false)
		if state.Recovered || state.Safety != "blocked_recover_gate_unavailable" {
			t.Fatalf("recover with deleted gate branch = %#v", state)
		}
	})
	t.Run("gate missing", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunCancelled)
		if err := os.RemoveAll(f.gate); err != nil {
			t.Fatal(err)
		}
		state := f.service.Recover(f.ctx, false)
		if state.Recovered || state.Safety != "blocked_recover_gate_unavailable" {
			t.Fatalf("recover with missing gate = %#v", state)
		}
		if f.custodyReturned() {
			t.Fatal("unverifiable preservation stamped custody")
		}
	})
}

// TestRecoverLegacySubmittedHeadMismatchKeepLocalPreservesBothHeads is the
// exact historical rebase-failure shape: the clean operator and mutable gate
// branch still name submitted A, while the terminal row records rebased R and
// R remains as an otherwise-unreferenced gate object. The bounded fallback
// must pin and byte-verify R without moving A, then return custody.
func TestRecoverLegacySubmittedHeadMismatchKeepLocalPreservesBothHeads(t *testing.T) {
	f := newRecoverFixture(t, types.RunFailed)
	makeLegacyRebaseMismatch(t, f)

	defaultState := f.service.Recover(f.ctx, false)
	if defaultState.Recovered || defaultState.Safety != "blocked_recover_diverged" || defaultState.Relation != RelationDiverged {
		t.Fatalf("legacy default recovery = %#v", defaultState)
	}
	if got := mustRun(t, f.gate, "rev-parse", f.runHeadRef()); got != f.preserved {
		t.Fatalf("legacy default did not preserve run head: %s", got)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("legacy default did not fetch exact recovery head: %s", got)
	}
	if f.custodyReturned() {
		t.Fatal("legacy default divergence stamped custody")
	}

	state := f.service.Recover(f.ctx, true)
	if !state.Recovered || state.Changed || state.Safety != "custody_returned" {
		t.Fatalf("legacy keep-local recover = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("local head = %s, want submitted %s", got, f.submitted)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("gate head = %s, want submitted %s", got, f.submitted)
	}
	for label, location := range map[string][2]string{
		"gate run head":  {f.gate, f.runHeadRef()},
		"local recovery": {f.local, f.anchorRef()},
	} {
		if got := mustRun(t, location[0], "rev-parse", location[1]); got != f.preserved {
			t.Fatalf("%s = %s, want preserved %s", label, got, f.preserved)
		}
	}
	if !f.custodyReturned() {
		t.Fatal("legacy keep-local recovery did not stamp custody")
	}
}

func TestRecoverLegacySubmittedHeadMismatchDirectKeepLocalPreservesBothHeads(t *testing.T) {
	f := newRecoverFixture(t, types.RunFailed)
	makeLegacyRebaseMismatch(t, f)

	state := f.service.Recover(f.ctx, true)
	if !state.Recovered || state.Changed || state.Safety != "custody_returned" {
		t.Fatalf("direct legacy keep-local recovery = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("direct keep-local moved local head to %s", got)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("direct keep-local moved submitted gate head to %s", got)
	}
	if got := mustRun(t, f.gate, "rev-parse", f.runHeadRef()); got != f.preserved {
		t.Fatalf("direct keep-local run ref = %s", got)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("direct keep-local recovery ref = %s", got)
	}
}

func TestRecoverLegacySubmittedHeadMismatchMissingObjectRefuses(t *testing.T) {
	f := newRecoverFixture(t, types.RunFailed)
	mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted, f.preserved)
	mustRun(t, f.gate, "reflog", "expire", "--expire=now", "--all")
	mustRun(t, f.gate, "gc", "--prune=now")
	if _, err := git.Run(f.ctx, f.gate, "cat-file", "-e", f.preserved+"^{commit}"); err == nil {
		t.Fatal("fixture preserved object still exists after prune")
	}

	state := f.service.Recover(f.ctx, true)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_preserve_failed" {
		t.Fatalf("missing-object recovery = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatal("missing-object recovery moved local head")
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatal("missing-object recovery moved gate branch")
	}
	if exists, _ := git.RefExists(f.ctx, f.gate, f.runHeadRef()); exists {
		t.Fatal("missing-object recovery created a run head ref")
	}
	if f.custodyReturned() {
		t.Fatal("missing-object recovery stamped custody")
	}
}

func TestRecoverLegacySubmittedHeadMismatchNewerActiveOwnerBlocks(t *testing.T) {
	f := newRecoverFixture(t, types.RunFailed)
	makeLegacyRebaseMismatch(t, f)
	active, err := f.db.InsertRun(f.repo.ID, "feature/recover", f.submitted, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunHeadSHA(active.ID, f.preserved); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	state := f.service.Recover(f.ctx, true)
	if state.Recovered || state.Safety != "blocked_recover_run_active" {
		t.Fatalf("legacy recovery with newer owner = %#v", state)
	}
	if exists, _ := git.RefExists(f.ctx, f.gate, f.runHeadRef()); exists {
		t.Fatal("legacy recovery pinned the old run while a newer owner was active")
	}
	if f.custodyReturned() {
		t.Fatal("legacy recovery stamped old custody while a newer owner was active")
	}
}

func TestRecoverLegacySubmittedHeadMismatchThirdGateHeadRefuses(t *testing.T) {
	f := newRecoverFixture(t, types.RunFailed)
	writer := filepath.Join(t.TempDir(), "writer")
	mustRun(t, filepath.Dir(writer), "clone", f.gate, writer)
	configureIdentity(t, writer)
	mustRun(t, writer, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(writer, "third.txt"), "third\n")
	mustRun(t, writer, "add", "third.txt")
	mustRun(t, writer, "commit", "-m", "unexplained third head")
	third := mustRun(t, writer, "rev-parse", "HEAD")
	mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")

	state := f.service.Recover(f.ctx, true)
	if state.Recovered || state.Safety != "blocked_recover_gate_diverged" {
		t.Fatalf("third-head recovery = %#v", state)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != third {
		t.Fatal("third-head recovery clobbered gate winner")
	}
	if exists, _ := git.RefExists(f.ctx, f.gate, f.runHeadRef()); exists {
		t.Fatal("third-head recovery treated loose object existence as authority")
	}
	if f.custodyReturned() {
		t.Fatal("third-head recovery stamped custody")
	}
}

func TestRecoverLegacySubmittedHeadMismatchRechecksGateAfterPin(t *testing.T) {
	f := newRecoverFixture(t, types.RunFailed)
	mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted, f.preserved)
	var winner string
	f.service.beforeLegacyRecoveryRecheck = func() {
		writer := filepath.Join(t.TempDir(), "racer")
		mustRun(t, filepath.Dir(writer), "clone", f.gate, writer)
		configureIdentity(t, writer)
		mustRun(t, writer, "checkout", "feature/recover")
		mustWrite(t, filepath.Join(writer, "race.txt"), "race\n")
		mustRun(t, writer, "add", "race.txt")
		mustRun(t, writer, "commit", "-m", "recovery race")
		winner = mustRun(t, writer, "rev-parse", "HEAD")
		mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
	}

	state := f.service.Recover(f.ctx, true)
	if state.Recovered || state.Safety != "blocked_recover_assumptions_changed" {
		t.Fatalf("racing legacy recovery = %#v", state)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != winner {
		t.Fatal("racing legacy recovery clobbered gate winner")
	}
	if f.custodyReturned() {
		t.Fatal("racing legacy recovery stamped custody")
	}
}

func TestAmbiguousPublishedRefBlocksFreshRunBeforeCleanupWhenDatabaseStayedSubmitted(t *testing.T) {
	f := newRecoverFixture(t, types.RunFailed)
	if err := f.db.UpdateRunHeadSHA(f.run.ID, f.submitted); err != nil {
		t.Fatal(err)
	}
	f.run.HeadSHA = f.submitted
	if err := git.PinRunHead(f.ctx, f.gate, f.run.ID, f.preserved); err != nil {
		t.Fatal(err)
	}

	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned_ambiguous" {
		t.Fatalf("submitted-authority ambiguous state = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code != "resolve_ambiguous_custody" {
		t.Fatalf("ambiguous next action = %#v", state.NextAction)
	}
	for _, candidate := range []string{f.submitted, f.preserved} {
		if !strings.Contains(state.Error, candidate) {
			t.Fatalf("ambiguous note does not name candidate %s: %q", candidate, state.Error)
		}
	}
	if recovered := f.service.Recover(f.ctx, true); recovered.Recovered || recovered.Safety != "blocked_recover_ambiguous_head" {
		t.Fatalf("ambiguous submitted-authority recover = %#v", recovered)
	}
}

// TestResolveAmbiguousHeadRequiresOperatorNamedCandidate proves the ambiguous
// state has exactly one supported exit and that the tool never chooses: an
// empty or non-candidate commit refuses without touching any ref, and the
// branch stays blocked.
func TestResolveAmbiguousHeadRequiresOperatorNamedCandidate(t *testing.T) {
	for name, chosen := range map[string]string{"empty": "", "not a candidate": "0000000000000000000000000000000000000000"} {
		t.Run(name, func(t *testing.T) {
			f := newAmbiguousFixture(t)
			state := f.service.ResolveAmbiguousHead(f.ctx, chosen)
			if state.Changed || state.Safety != "blocked_resolve_head_not_candidate" {
				t.Fatalf("resolution = %#v", state)
			}
			for _, candidate := range []string{f.submitted, f.preserved} {
				if !strings.Contains(state.Error, candidate) {
					t.Fatalf("refusal does not list candidate %s: %q", candidate, state.Error)
				}
			}
			if got := mustRun(t, f.gate, "rev-parse", f.runHeadRef()); got != f.preserved {
				t.Fatalf("refused resolution moved the run ref to %s", got)
			}
			if reloaded, _ := f.db.GetRun(f.run.ID); reloaded.HeadSHA != f.submitted {
				t.Fatalf("refused resolution moved durable authority to %s", reloaded.HeadSHA)
			}
		})
	}
}

// TestResolveAmbiguousHeadPublishesNamedCommitAndArchivesLosers is the exit
// itself: naming the preserved pipeline head publishes it as run authority,
// archives the losing head rather than discarding it, and leaves the branch
// ordinarily recoverable.
func TestResolveAmbiguousHeadPublishesNamedCommitAndArchivesLosers(t *testing.T) {
	f := newAmbiguousFixture(t)
	state := f.service.ResolveAmbiguousHead(f.ctx, f.preserved)
	if !state.Changed || state.Safety != "blocked_pipeline_owned_recoverable" {
		t.Fatalf("resolution = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code != "recover_custody" {
		t.Fatalf("resolved next action = %#v", state.NextAction)
	}
	if got := mustRun(t, f.gate, "rev-parse", f.runHeadRef()); got != f.preserved {
		t.Fatalf("run ref = %s, want the named head %s", got, f.preserved)
	}
	reloaded, _ := f.db.GetRun(f.run.ID)
	if reloaded.HeadSHA != f.preserved {
		t.Fatalf("durable authority = %s, want the named head %s", reloaded.HeadSHA, f.preserved)
	}
	if got := mustRun(t, f.gate, "rev-parse", git.ArchiveHeadRef(f.run.ID, f.submitted)); got != f.submitted {
		t.Fatalf("losing head was not archived: %s", got)
	}
	// The whole point is that resolution is not destructive: it may not move a
	// branch, touch files, or stamp custody on the operator's behalf.
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("resolution moved the operator worktree to %s", got)
	}
	if f.custodyReturned() {
		t.Fatal("resolution stamped custody instead of leaving the guarded recovery to the operator")
	}

	// The branch is now genuinely usable again: ordinary recovery proceeds.
	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered {
		t.Fatalf("recovery after resolution = %#v", recovered)
	}
}

// TestResolveAmbiguousHeadRetiresCrashCandidateAfterArchiving covers the crash
// shape: a fix agent self-committed, cleanup pinned the live head as crash
// evidence, and the operator keeps the recorded head. The crash ref is retired
// only after its commit is archived.
func TestResolveAmbiguousHeadRetiresCrashCandidateAfterArchiving(t *testing.T) {
	f := newRecoverFixture(t, types.RunFailed)
	crash := mustRun(t, f.pipeline, "commit-tree", f.preserved+"^{tree}", "-p", f.preserved, "-m", "unrecorded agent self-commit")
	mustRun(t, f.pipeline, "push", f.gate, crash+":refs/no-mistakes/crash-stage")
	mustRun(t, f.gate, "update-ref", "-d", "refs/no-mistakes/crash-stage")
	if err := git.PinRunHead(f.ctx, f.gate, f.run.ID, f.preserved); err != nil {
		t.Fatal(err)
	}
	if err := git.PinExactCommit(f.ctx, f.gate, git.CrashHeadRef(f.run.ID), crash); err != nil {
		t.Fatal(err)
	}

	if state := f.service.InspectCached(f.ctx); state.Safety != "blocked_pipeline_owned_ambiguous" {
		t.Fatalf("crash-candidate state = %#v", state)
	}
	state := f.service.ResolveAmbiguousHead(f.ctx, f.preserved)
	if !state.Changed || state.Safety != "blocked_pipeline_owned_recoverable" {
		t.Fatalf("crash-candidate resolution = %#v", state)
	}
	if got := mustRun(t, f.gate, "rev-parse", git.ArchiveHeadRef(f.run.ID, crash)); got != crash {
		t.Fatalf("crash candidate was retired without archiving: %s", got)
	}
	if exists, _ := git.RefExists(f.ctx, f.gate, git.CrashHeadRef(f.run.ID)); exists {
		t.Fatal("crash candidate ref was not retired after resolution")
	}
}

// TestUnreadablePreservationEvidenceFailsClosed pins the direction this guard
// must fail in. When the gate's preservation refs cannot be read, ambiguity
// cannot be ruled out, so inspection, recovery, and resolution must all refuse
// rather than silently treat the branch as unambiguous and stamp custody.
func TestUnreadablePreservationEvidenceFailsClosed(t *testing.T) {
	f := newAmbiguousFixture(t)
	// The gate path exists but its refs cannot be read. This is the unreadable
	// case, distinct from the absent-gate case that the gate-unavailable
	// refusals already own.
	unreadable := filepath.Join(t.TempDir(), "unreadable-gate")
	if err := os.MkdirAll(unreadable, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git.PreservedHeads(f.ctx, unreadable); err == nil {
		t.Fatal("fixture gate is still readable")
	}
	service := &Service{DB: f.db, Repo: f.repo, WorkDir: f.local, GateDir: unreadable}

	state := service.InspectCached(f.ctx)
	if state.Safety != "blocked_preservation_unreadable" {
		t.Fatalf("unreadable evidence inspection = %#v", state)
	}
	for _, keepLocal := range []bool{false, true} {
		recovered := service.Recover(f.ctx, keepLocal)
		if recovered.Recovered || recovered.Safety != "blocked_recover_preservation_unreadable" {
			t.Fatalf("keep_local=%v unreadable evidence recovery = %#v", keepLocal, recovered)
		}
		if f.custodyReturned() {
			t.Fatalf("keep_local=%v stamped custody on unreadable preservation evidence", keepLocal)
		}
	}
	resolved := service.ResolveAmbiguousHead(f.ctx, f.preserved)
	if resolved.Changed || resolved.Safety != "blocked_resolve_preservation_unreadable" {
		t.Fatalf("unreadable evidence resolution = %#v", resolved)
	}
	// The readable gate still holds every head, so nothing was discarded while
	// the branch was blocked.
	if got := mustRun(t, f.gate, "rev-parse", f.runHeadRef()); got != f.preserved {
		t.Fatalf("blocked state disturbed the preserved run ref: %s", got)
	}
}

// TestRecoverPinnedRunRefStillRefusesThirdGateHead is the regression for the
// widening this arm introduced: a byte-verified run ref must not license
// recovery across a live third gate head, because --keep-local would then
// compare-and-swap that head off the gate branch without ever naming it.
func TestRecoverPinnedRunRefStillRefusesThirdGateHead(t *testing.T) {
	f := newRecoverFixture(t, types.RunFailed)
	if err := git.PinRunHead(f.ctx, f.gate, f.run.ID, f.preserved); err != nil {
		t.Fatal(err)
	}
	writer := filepath.Join(t.TempDir(), "writer")
	mustRun(t, filepath.Dir(writer), "clone", f.gate, writer)
	configureIdentity(t, writer)
	mustRun(t, writer, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(writer, "third.txt"), "colleague work\n")
	mustRun(t, writer, "add", "third.txt")
	mustRun(t, writer, "commit", "-m", "colleague pushed tip")
	third := mustRun(t, writer, "rev-parse", "HEAD")
	mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")

	for _, keepLocal := range []bool{false, true} {
		state := f.service.Recover(f.ctx, keepLocal)
		if state.Recovered || state.Safety != "blocked_recover_gate_diverged" {
			t.Fatalf("keep_local=%v third-head recovery = %#v", keepLocal, state)
		}
		if !strings.Contains(state.Error, third) {
			t.Fatalf("refusal does not name the displaced gate head %s: %q", third, state.Error)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != third {
			t.Fatalf("keep_local=%v displaced the colleague tip to %s", keepLocal, got)
		}
		if f.custodyReturned() {
			t.Fatalf("keep_local=%v stamped custody across a third gate head", keepLocal)
		}
	}
	if got := mustRun(t, f.gate, "rev-parse", f.runHeadRef()); got != f.preserved {
		t.Fatalf("refusal disturbed the preserved run ref: %s", got)
	}
}

// TestResolveAmbiguousHeadCreatesRunAuthorityFromCrashEvidenceOnly covers the
// shape where preservation pinned crash evidence but no run-owned ref: naming
// the crash head must create that ref from absent and move durable authority,
// both with compare-and-swap, while the losing recorded head is archived.
func TestResolveAmbiguousHeadCreatesRunAuthorityFromCrashEvidenceOnly(t *testing.T) {
	f := newRecoverFixture(t, types.RunFailed)
	if err := f.db.UpdateRunHeadSHA(f.run.ID, f.submitted); err != nil {
		t.Fatal(err)
	}
	f.run.HeadSHA = f.submitted
	if err := git.PinExactCommit(f.ctx, f.gate, git.CrashHeadRef(f.run.ID), f.preserved); err != nil {
		t.Fatal(err)
	}

	state := f.service.ResolveAmbiguousHead(f.ctx, f.preserved)
	if !state.Changed || state.Safety != "blocked_pipeline_owned_recoverable" {
		t.Fatalf("crash-only resolution = %#v", state)
	}
	if got := mustRun(t, f.gate, "rev-parse", f.runHeadRef()); got != f.preserved {
		t.Fatalf("run ref = %s, want the named head %s", got, f.preserved)
	}
	reloaded, _ := f.db.GetRun(f.run.ID)
	if reloaded.HeadSHA != f.preserved {
		t.Fatalf("durable authority = %s, want %s", reloaded.HeadSHA, f.preserved)
	}
	if got := mustRun(t, f.gate, "rev-parse", git.ArchiveHeadRef(f.run.ID, f.submitted)); got != f.submitted {
		t.Fatalf("losing recorded head was not archived: %s", got)
	}
	if exists, _ := git.RefExists(f.ctx, f.gate, git.CrashHeadRef(f.run.ID)); exists {
		t.Fatal("crash evidence ref was not retired after resolution")
	}
}

// TestRecoverLegacySubmittedHeadMismatchIsIdempotent pins the DEV-451 legacy
// shape against a re-run: the first recovery pins the exact run ref, and the
// second must return the same actionable refusal rather than degrading to a
// weaker gate-diverged message with no --keep-local exit.
func TestRecoverLegacySubmittedHeadMismatchIsIdempotent(t *testing.T) {
	f := newRecoverFixture(t, types.RunFailed)
	makeLegacyRebaseMismatch(t, f)

	first := f.service.Recover(f.ctx, false)
	second := f.service.Recover(f.ctx, false)
	if first.Safety != "blocked_recover_diverged" || second.Safety != first.Safety {
		t.Fatalf("legacy recovery is not idempotent: first %s, second %s", first.Safety, second.Safety)
	}
	if second.NextAction == nil || second.NextAction.Code != "inspect_and_reconcile_manually" {
		t.Fatalf("second legacy refusal next action = %#v", second.NextAction)
	}
	if !strings.Contains(second.Error, "--keep-local") {
		t.Fatalf("second legacy refusal dropped the --keep-local exit: %q", second.Error)
	}
	// rerun refuses in this shape (the gate branch is not the preserved head),
	// so recovery must not advertise it.
	if strings.Contains(second.Error, "no-mistakes rerun") {
		t.Fatalf("legacy refusal offered unusable rerun advice: %q", second.Error)
	}

	keepLocal := f.service.Recover(f.ctx, true)
	if !keepLocal.Recovered || keepLocal.Changed {
		t.Fatalf("keep-local after repeated refusals = %#v", keepLocal)
	}
	if got := mustRun(t, f.gate, "rev-parse", f.runHeadRef()); got != f.preserved {
		t.Fatalf("idempotent legacy recovery lost the preserved head: %s", got)
	}
}

// newAmbiguousFixture models the reachable ambiguity: a Git publication landed
// under the run-owned ref while the database write never did, so evidence and
// durable authority name different heads.
func newAmbiguousFixture(t *testing.T) *recoverFixture {
	t.Helper()
	f := newRecoverFixture(t, types.RunFailed)
	if err := f.db.UpdateRunHeadSHA(f.run.ID, f.submitted); err != nil {
		t.Fatal(err)
	}
	f.run.HeadSHA = f.submitted
	if err := git.PinRunHead(f.ctx, f.gate, f.run.ID, f.preserved); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestRecoverTerminalPostPushRunWithMovedHead covers the post-push class cell:
// a run that pushed successfully, then went terminal with additional
// unpublished pipeline commits. Recovery fast-forwards to the preserved head
// and the branch classifies as local_ahead against the pushed binding, whose
// existing run_pipeline guidance publishes the recovered commits.
func TestRecoverTerminalPostPushRunWithMovedHead(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	// The run pushed the submitted head upstream, then the pipeline moved on.
	mustRun(t, f.local, "push", f.remote, "refs/heads/feature/recover:refs/heads/feature/recover")
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
		HeadSHA: f.submitted, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover",
	}); err != nil {
		t.Fatal(err)
	}

	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned_recoverable" {
		t.Fatalf("post-push terminal state = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code != "recover_custody" {
		t.Fatalf("post-push next action = %#v", state.NextAction)
	}

	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || !recovered.Changed {
		t.Fatalf("post-push recover = %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("post-push recover HEAD = %s, want %s", got, f.preserved)
	}
	if recovered.State != StateLocalAhead || recovered.NextAction == nil || recovered.NextAction.Code != "run_pipeline" {
		t.Fatalf("post-push recovered classification = %#v", recovered)
	}
	if !f.custodyReturned() {
		t.Fatal("post-push recover did not stamp custody")
	}
}

// TestRecoverRefusesWhenNothingIsStranded pins the not-applicable guard: a
// healthy behind state (successful push binding) must not be recoverable.
func TestRecoverRefusesWhenNothingIsStranded(t *testing.T) {
	f := newSyncFixture(t)
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Safety != "blocked_recover_not_applicable" {
		t.Fatalf("recover on healthy state = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.old {
		t.Fatal("not-applicable refusal moved HEAD")
	}
}

// TestRecoverConcurrentGatePushLosesCleanly: the keep-local gate reset is an
// atomic compare-and-swap, so a racing push to the gate wins and recovery
// refuses instead of clobbering the newer gate head.
func TestRecoverConcurrentGatePushLosesCleanly(t *testing.T) {
	f := newRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "rescope.txt"), "rescope\n")
	mustRun(t, f.local, "add", "rescope.txt")
	mustRun(t, f.local, "commit", "-m", "diverging rescope")
	f.service.beforeGateReset = func() {
		writer := filepath.Join(t.TempDir(), "racer")
		mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
		configureIdentity(t, writer)
		mustRun(t, writer, "checkout", "feature/recover")
		mustWrite(t, filepath.Join(writer, "race.txt"), "race\n")
		mustRun(t, writer, "add", "race.txt")
		mustRun(t, writer, "commit", "-m", "racing push")
		mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
	}
	state := f.service.Recover(f.ctx, true)
	if state.Recovered || state.Safety != "blocked_recover_gate_race" {
		t.Fatalf("racing keep-local recover = %#v", state)
	}
	if f.custodyReturned() {
		t.Fatal("racing recover stamped custody")
	}
}
