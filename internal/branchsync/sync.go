package branchsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const refreshTimeout = 15 * time.Second

const (
	StatePipelineOwned        = "pipeline_owned"
	StatePushInProgress       = "push_in_progress"
	StateBehind               = "behind"
	StateSynchronized         = "synchronized"
	StateLocalAhead           = "local_ahead"
	StateDiverged             = "diverged"
	StateDirty                = "dirty"
	StateRemoteAdvanced       = "remote_advanced"
	StateRemoteRewritten      = "remote_rewritten"
	StateRemoteMissing        = "remote_missing"
	StateMergedRemoteRetained = "merged_remote_retained"
	StateMergedRemoteRemoved  = "merged_remote_removed"
	StateClosed               = "closed"
	StateOffline              = "offline"
	StateTargetChanged        = "target_changed"
	StateAmbiguousContext     = "ambiguous_context"
	StateLegacyUnbound        = "legacy_unbound"
	StateCustodyReturned      = "custody_returned"
)

const (
	RelationEqual    = "equal"
	RelationBehind   = "behind"
	RelationAhead    = "ahead"
	RelationDiverged = "diverged"
	RelationUnknown  = "unknown"
)

const (
	SafetySafeFastForward       = "safe_fast_forward"
	SafetySafeEquivalentAdvance = "safe_equivalent_advance"
)

// State is the shared branch synchronization contract rendered by CLI, AXI,
// and TUI presenters. Cached inspection never contacts a remote.
type State struct {
	State    string
	Changed  bool
	Local    LocalState
	Pipeline PipelineState
	Target   TargetState
	Remote   RemoteState
	Relation string
	Safety   string
	PRState  string
	// Recovered is set only by Recover: custody of the stranded terminal run
	// was returned (either by this call or by an earlier, idempotent one).
	Recovered  bool
	NextAction *NextAction
	Error      string
}

type LocalState struct {
	Branch string
	Head   string
	Clean  bool
	Reason string
}

type PipelineState struct {
	RunID          string
	Status         string
	Phase          string
	SubmittedHead  string
	CurrentHead    string
	PushedHead     string
	PushedAt       int64
	PushGeneration int64
}

type TargetState struct {
	Kind   string
	Remote string
	URL    string
	Ref    string
}

type RemoteState struct {
	ObservedHead string
	Freshness    string
	ObservedAt   int64
}

type NextAction struct {
	Code    string
	Command string
}

// CanApply reports whether Apply may advance the clean checked-out branch for
// a freshly verified plan. It includes strict fast-forwards and the narrower
// equivalent-diverged advance that first anchors the pre-sync head.
func CanApply(state State) bool {
	return state.Safety == SafetySafeFastForward || state.Safety == SafetySafeEquivalentAdvance
}

// Service synchronizes only the invoking worktree. Repo is the registered
// repository record, while WorkDir may be its main or a linked worktree.
// GateDir is the repo's local bare gate; Recover reads preserved pipeline
// heads from it and is the only method that touches it.
type Service struct {
	DB      *db.DB
	Repo    *db.Repo
	WorkDir string
	GateDir string
	Paths   *paths.Paths

	beforeApply                 func()
	beforeGateReset             func()
	beforeRecoverFastForward    func()
	beforeLegacyRecoveryRecheck func()
}

// OpenCurrent opens a service for the invoking registered worktree. The caller
// owns the returned close function.
func OpenCurrent() (*Service, func(), error) {
	p, err := paths.New()
	if err != nil {
		return nil, nil, err
	}
	database, err := db.Open(p.DB())
	if err != nil {
		return nil, nil, err
	}
	root, err := git.FindGitRoot(".")
	if err != nil {
		database.Close()
		return nil, nil, fmt.Errorf("not in a git repository")
	}
	repo, err := database.GetRepoByPath(root)
	if err != nil {
		database.Close()
		return nil, nil, err
	}
	if repo == nil {
		mainRoot, mainErr := git.FindMainRepoRoot(root)
		if mainErr == nil {
			repo, err = database.GetRepoByPath(mainRoot)
		}
	}
	if err != nil || repo == nil {
		database.Close()
		return nil, nil, fmt.Errorf("repo not initialized")
	}
	return &Service{DB: database, Repo: repo, WorkDir: root, GateDir: p.RepoDir(repo.ID), Paths: p}, func() { _ = database.Close() }, nil
}

// TargetFingerprint returns a stable one-way identity for a credential-free,
// canonical target. No URL is persisted by callers.
func TargetFingerprint(raw string) string {
	sum := sha256.Sum256([]byte(canonicalTarget(raw)))
	return hex.EncodeToString(sum[:])
}

func canonicalTarget(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Scheme != "" {
		if parsed.Scheme == "http" || parsed.Scheme == "https" {
			parsed.User = nil
			parsed.Scheme = strings.ToLower(parsed.Scheme)
			parsed.Host = strings.ToLower(parsed.Host)
		}
		parsed.Fragment = ""
		return strings.TrimSuffix(parsed.String(), "/")
	}
	return strings.TrimSuffix(raw, "/")
}

func displayTarget(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		parsed.User = nil
		return parsed.String()
	}
	return safeurl.Redact(raw)
}

// InspectCached reads local Git and persisted provenance without fetching or
// mutating refs, the index, or the worktree.
func (s *Service) InspectCached(ctx context.Context) State {
	state, _, _ := s.inspect(ctx)
	return state
}

// Refresh explicitly verifies the exact configured push ref into a private
// no-mistakes ref. It never updates an ordinary remote-tracking ref.
func (s *Service) Refresh(ctx context.Context) State {
	state, run, ok := s.inspect(ctx)
	if !ok || !refreshable(state) {
		return state
	}
	freshRun, runErr := s.DB.GetRun(run.ID)
	freshRepo, repoErr := s.DB.GetRepo(s.Repo.ID)
	if runErr != nil || repoErr != nil || freshRun == nil || freshRepo == nil || freshRun.PushActive ||
		value(freshRun.PushGeneration) != state.Pipeline.PushGeneration || ptr(freshRun.LastPushedSHA) != state.Pipeline.PushedHead ||
		ptr(freshRun.PushTargetFingerprint) != TargetFingerprint(freshRepo.PushURL()) || ptr(freshRun.PushTargetKind) != targetKind(freshRepo) || ptr(freshRun.PushRef) != state.Target.Ref {
		if state.PRState == "merged" || state.PRState == "closed" {
			return state
		}
		return blockedPlan(state, StateTargetChanged, "blocked_binding_changed", "the push binding or configured target changed before refresh; no files or refs were changed")
	}
	pushURL := freshRepo.PushURL()

	refreshCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	live, err := git.LsRemote(refreshCtx, s.workDir(), pushURL, state.Target.Ref)
	if err != nil {
		state.State = StateOffline
		state.Safety = "blocked_offline"
		state.Error = "could not refresh the configured push target; no files or refs were changed"
		state.NextAction = &NextAction{Code: "retry", Command: "no-mistakes sync --check"}
		return state
	}
	state.Remote.Freshness = "live"
	state.Remote.ObservedAt = time.Now().Unix()
	state.Remote.ObservedHead = live

	if live == "" {
		state.Relation = RelationUnknown
		state.NextAction = nil
		if state.PRState == "merged" {
			state.State = StateMergedRemoteRemoved
			state.Safety = "already_retired"
			state.Error = ""
			return state
		}
		if state.PRState == "closed" {
			state.State = StateClosed
			state.Safety = "blocked_closed"
			return state
		}
		state.State = StateRemoteMissing
		state.Safety = "blocked_remote_missing"
		state.Error = "the pipeline-bound remote branch no longer exists; no files or refs were changed"
		return state
	}

	privateRef := "refs/no-mistakes/sync/" + run.ID
	branch := strings.TrimPrefix(state.Target.Ref, "refs/heads/")
	if err := git.FetchRemoteBranchToPrivateRef(refreshCtx, s.workDir(), pushURL, branch, privateRef); err != nil {
		state.State = StateOffline
		state.Safety = "blocked_offline"
		state.Error = "could not fetch the configured push target; no files or worktree refs were changed"
		return state
	}
	fetched, err := git.Run(ctx, s.workDir(), "rev-parse", privateRef)
	if err != nil || fetched != live {
		state.State = StateRemoteRewritten
		state.Safety = "blocked_remote_changed_during_refresh"
		state.Error = "the remote branch changed while it was being refreshed; no files or worktree refs were changed"
		return state
	}

	bound := ptr(run.LastPushedSHA)
	if live != bound {
		state.NextAction = nil
		if isAncestor(ctx, s.workDir(), bound, live) {
			state.State = StateRemoteAdvanced
			state.Safety = "blocked_remote_advanced"
			state.Relation = RelationUnknown
			state.Error = "the live remote contains commits outside the persisted pipeline push binding; no files or refs were changed"
		} else {
			state.State = StateRemoteRewritten
			state.Safety = "blocked_remote_rewritten"
			state.Relation = RelationUnknown
			state.Error = "the live remote no longer equals the persisted pipeline push binding; no files or refs were changed"
		}
		return state
	}

	if state.PRState == "merged" {
		state.State = StateMergedRemoteRetained
		state.Safety = "blocked_merged"
		state.NextAction = nil
		return state
	}
	if state.PRState == "closed" {
		state.State = StateClosed
		state.Safety = "blocked_closed"
		state.NextAction = nil
		return state
	}

	s.classifyRelation(ctx, &state, bound, run.BaseSHA, true)
	return state
}

func (s *Service) gateContextRefusal(ctx context.Context) (State, bool) {
	p := s.Paths
	if p == nil && strings.TrimSpace(s.GateDir) != "" {
		p = paths.WithRoot(filepath.Dir(filepath.Dir(filepath.Clean(s.GateDir))))
	}
	if p == nil {
		// Manually constructed services without a gate path are used by pure
		// branch-sync callers and tests. Production entrypoints always provide
		// Paths/GateDir and are classified before mutation.
		return State{}, false
	}
	result, err := (gatecontext.Inspector{DB: s.DB, Paths: p}).Inspect(ctx, gatecontext.Request{CWD: s.workDir(), MarkerPresent: gatecontext.MarkerPresent()})
	if err != nil {
		return State{State: StateAmbiguousContext, Safety: "blocked_gate_context_unknown", Error: "could not verify gate execution context; no files or refs were changed"}, true
	}
	if !result.Nested {
		return State{}, false
	}
	return State{State: StateAmbiguousContext, Safety: gatecontext.ErrorCode, Error: gatecontext.RefusalMessage(result)}, true
}

// Apply repeats remote and mutable-precondition checks, then advances the clean
// checked-out branch to the exact pipeline-bound SHA. Ordinary behind branches
// use a strict fast-forward. Equivalent-diverged branches first anchor the
// pre-sync head, then move to the verified equivalent pipeline head.
func (s *Service) Apply(ctx context.Context) State {
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return refusal
	}
	plan := s.Refresh(ctx)
	if plan.State == StateSynchronized || plan.State == StateMergedRemoteRemoved {
		plan.Changed = false
		return plan
	}
	if !CanApply(plan) {
		return plan
	}
	if s.beforeApply != nil {
		s.beforeApply()
	}

	freshRun, err := s.DB.GetRun(plan.Pipeline.RunID)
	freshRepo, repoErr := s.DB.GetRepo(s.Repo.ID)
	if err != nil || repoErr != nil || freshRepo == nil || freshRun == nil || freshRun.PushActive || ptr(freshRun.LastPushedSHA) != plan.Pipeline.PushedHead ||
		value(freshRun.PushGeneration) != plan.Pipeline.PushGeneration || ptr(freshRun.PushRef) != plan.Target.Ref ||
		ptr(freshRun.PushTargetFingerprint) != TargetFingerprint(freshRepo.PushURL()) || ptr(freshRun.PushTargetKind) != targetKind(freshRepo) {
		return blockedPlan(plan, "pipeline_owned", "blocked_generation_changed", "the pipeline push binding changed before synchronization; no files or refs were changed")
	}

	recheck, _, ok := s.inspect(ctx)
	if !ok || recheck.Local.Head != plan.Local.Head || !recheck.Local.Clean || recheck.Local.Branch != plan.Local.Branch {
		return blockedPlan(recheck, StateAmbiguousContext, "blocked_assumptions_changed", "the local branch or worktree changed before synchronization; no files or refs were changed")
	}
	if recheck.State == StatePushInProgress || recheck.State == StatePipelineOwned || recheck.State == StateDirty {
		return recheck
	}

	checkCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	live, err := git.LsRemote(checkCtx, s.workDir(), s.Repo.PushURL(), plan.Target.Ref)
	if err != nil || live != plan.Pipeline.PushedHead {
		return blockedPlan(plan, StateRemoteRewritten, "blocked_remote_changed_before_apply", "the live remote changed before synchronization; no files or refs were changed")
	}
	finalPrecondition, finalRun, finalOK := s.inspect(ctx)
	finalRepo, finalRepoErr := s.DB.GetRepo(s.Repo.ID)
	if !finalOK || finalRun == nil || finalRepoErr != nil || finalRepo == nil || finalRun.PushActive ||
		value(finalRun.PushGeneration) != plan.Pipeline.PushGeneration || ptr(finalRun.PushTargetFingerprint) != TargetFingerprint(finalRepo.PushURL()) || ptr(finalRun.PushTargetKind) != targetKind(finalRepo) ||
		finalPrecondition.Local.Branch != plan.Local.Branch || finalPrecondition.Local.Head != plan.Local.Head || !finalPrecondition.Local.Clean {
		return blockedPlan(finalPrecondition, StateAmbiguousContext, "blocked_assumptions_changed", "the push binding, branch, HEAD, or worktree changed immediately before synchronization; no files or refs were changed")
	}
	equivalentAdvance := plan.Safety == SafetySafeEquivalentAdvance
	if equivalentAdvance {
		if !equivalentDivergence(ctx, s.workDir(), plan.Local.Head, plan.Pipeline.PushedHead, finalRun.BaseSHA) {
			return blockedPlan(plan, StateDiverged, "blocked_diverged", "the equivalent-diverged proof changed before synchronization; no files or refs were changed")
		}
	} else if !isAncestor(ctx, s.workDir(), plan.Local.Head, plan.Pipeline.PushedHead) || plan.Local.Head == plan.Pipeline.PushedHead {
		return blockedPlan(plan, StateAmbiguousContext, "blocked_assumptions_changed", "the strict fast-forward assumptions changed before synchronization; no files or refs were changed")
	}

	var applyErr error
	if equivalentAdvance {
		anchorRef := syncAnchorRef(plan.Pipeline.RunID)
		if _, err := git.Run(ctx, s.workDir(), "update-ref", anchorRef, plan.Local.Head); err != nil {
			return blockedPlan(plan, StateAmbiguousContext, "blocked_preserve_failed", "the pre-sync local head could not be anchored; no files or refs were changed")
		}
		if anchored, err := git.Run(ctx, s.workDir(), "rev-parse", anchorRef+"^{commit}"); err != nil || anchored != plan.Local.Head {
			return blockedPlan(plan, StateAmbiguousContext, "blocked_preserve_failed", "the pre-sync local head could not be verified after anchoring; no files or worktree refs were changed")
		}
		_, applyErr = git.Run(ctx, s.workDir(), "reset", "--hard", plan.Pipeline.PushedHead)
	} else {
		_, applyErr = git.Run(ctx, s.workDir(), "merge", "--ff-only", "--no-edit", plan.Pipeline.PushedHead)
	}
	finalHead, _ := git.HeadSHA(ctx, s.workDir())
	finalClean, finalReason := worktreeClean(ctx, s.workDir())
	plan.Local.Head = finalHead
	plan.Local.Clean = finalClean
	plan.Local.Reason = finalReason
	plan.Changed = finalHead == plan.Pipeline.PushedHead && finalHead != recheck.Local.Head
	if applyErr != nil || finalHead != plan.Pipeline.PushedHead {
		plan.State = StateAmbiguousContext
		plan.Safety = "blocked_apply_failed"
		plan.Error = fmt.Sprintf("synchronization failed; final HEAD is %s and no destructive recovery was attempted", finalHead)
		return plan
	}
	if !finalClean {
		plan.State = StateDirty
		plan.Relation = RelationEqual
		plan.Safety = "blocked_post_apply_" + finalReason
		plan.Error = "HEAD reached the exact pipeline-pushed commit, but a Git hook left the worktree non-clean; no recovery was attempted"
		return plan
	}
	plan.State = StateSynchronized
	plan.Relation = RelationEqual
	plan.Safety = "already_synchronized"
	plan.NextAction = nil
	plan.Error = ""
	return plan
}

// Recover returns custody of a branch stranded by a TERMINAL run whose
// pipeline head was never published: cancelled or failed before the push, or
// terminal after a push with additional unpublished commits. While such a run
// was active the pipeline_owned block was correct; once it is terminal nothing
// will ever publish the head, so an explicit guarded exit must exist.
//
// The decision matrix is by worktree relation to preserved pipeline head P,
// defined as runs.head_sha byte-verified at refs/no-mistakes/run-head/<run>:
//
//	relation   worktree  default                        --keep-local
//	equal      any       anchor locally; return custody same
//	ahead      any       anchor locally; return custody same
//	behind     clean     strict fast-forward to P,      custody at local head;
//	                     then return custody            gate reset to it (CAS)
//	behind     dirty     refuse (commit/stash first)    custody at local head;
//	                                                    gate reset to it (CAS)
//	diverged   any       refuse (anchor named, manual   custody at local head;
//	                     reconcile / rerun offered)     gate reset/adoption only
//	                                                    after patch containment
//	P missing  any       refuse                         refuse
//
// Fail-safe rules, in the same spirit as Refresh/Apply:
//   - An active run always refuses: only terminal runs are recoverable.
//   - The preserved commits must be provably safe before custody moves: when
//     already reachable from the local branch (equal/ahead), recovery pins the
//     private anchor ref refs/no-mistakes/recover/<runID> locally without gate
//     access; otherwise it verifies and fetches the exact run-owned gate ref.
//     That ref must be byte-equal to runs.head_sha, and the mutable gate branch
//     must additionally be explainable: the preserved head; the run's own
//     immutable submitted head in the historical mismatch shape; or, for
//     explicit --keep-local only, a third gate head that is archived first and
//     whose preserved run-owned commits are all present in the current head by
//     patch content despite changed commit IDs. A third gate head without that
//     complete containment proof is refused and names the missing preserved
//     commits rather than being displaced.
//   - Preservation evidence that names more than one head, or that cannot be
//     read at all, is never resolved automatically. It blocks with
//     blocked_recover_ambiguous_head or blocked_recover_preservation_unreadable
//     until ResolveAmbiguousHead publishes an operator-named exact commit.
//   - The only possible worktree mutation stays a strict fast-forward of a
//     clean checked-out branch. When the operator explicitly keeps a behind or
//     diverged local head instead of taking P, --keep-local never touches the
//     worktree and moves the gate branch to the kept head with an atomic
//     compare-and-swap, so a concurrent gate push wins and recovery refuses.
//     If that kept head is a rebased/current copy of the preserved content, the
//     old gate head and original preserved head are archived under
//     refs/no-mistakes/recover/ before durable run authority moves to the
//     current head and custody is stamped.
//   - Anything unverifiable (missing object/ref, unexplained third gate head,
//     ambiguous crash candidate, failed anchor write/fetch, active owner, or
//     changed assumptions) refuses. Newly created exact preservation refs are
//     retained on a late race; files, mutable refs, and custody stay unchanged.
//
// Recovery ends with a persisted custody-return stamp on the run; inspection
// then reports custody_returned (never-pushed runs) or the ordinary
// classification against the last push binding (pushed runs), both pointing at
// run_pipeline as the next step. `no-mistakes rerun` resumes P only while the
// exact run ref and mutable gate branch agree; otherwise it refuses and directs
// the operator back to guarded custody recovery.
func (s *Service) Recover(ctx context.Context, keepLocal bool) State {
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return refusal
	}
	state, run, _ := s.inspect(ctx)
	if PreservationUnreadable(state) {
		return blockedPlan(state, StateAmbiguousContext, "blocked_recover_preservation_unreadable", preservationUnreadableMessage)
	}
	if run != nil && run.CustodyReturnedAt != nil {
		state.Recovered = true
		state.Changed = false
		return state
	}
	if state.State != StatePipelineOwned || run == nil {
		return blockedPlan(state, state.State, "blocked_recover_not_applicable", "nothing to recover: the branch is not held by a terminal run with unpublished pipeline commits; no files or refs were changed")
	}
	if !terminalRunStatus(run.Status) {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_run_active", "the run that owns this branch is still active; drive it to completion or abort it first; no files or refs were changed")
	}

	wd := s.workDir()
	branch := state.Local.Branch
	local := state.Local.Head
	preserved := strings.TrimSpace(run.HeadSHA)
	anchorRef := recoverAnchorRef(run.ID)
	refs, refsErr := s.loadPreservedHeads(ctx)
	if refsErr != nil {
		return blockedPlan(state, StateAmbiguousContext, "blocked_recover_preservation_unreadable", preservationUnreadableMessage)
	}
	if ambiguous, exists := ambiguousPreservedHead(refs, run); exists {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_ambiguous_head", fmt.Sprintf("preservation evidence names head %s in addition to recorded head %s; neither head was discarded, so resolve the ambiguity by naming the exact commit to keep with `no-mistakes sync --recover --resolve-head <commit>`", ambiguous, preserved))
	}

	if objectExists(ctx, wd, preserved) && (local == preserved || isAncestor(ctx, wd, preserved, local)) {
		if blocked, ok := s.anchorReachablePreserved(ctx, state, anchorRef, preserved); !ok {
			return blocked
		}
		if !s.localRecoveryAssumptionsStillExact(ctx, run, state) {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch, worktree, run authority, or active owner changed while the exact head was being anchored; the preservation ref was retained and custody was not returned")
		}
		return s.finishRecover(ctx, run, false)
	}

	gateDir := strings.TrimSpace(s.GateDir)
	if gateDir == "" {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", "no local gate is configured for this repository, so the preserved pipeline head cannot be verified; no files or refs were changed")
	}
	gateHead, err := git.ResolveRef(ctx, gateDir, "refs/heads/"+branch)
	if err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", fmt.Sprintf("the local gate no longer has branch %s, so the preserved pipeline head %s cannot be verified; no files or refs were changed", branch, preserved))
	}

	runRef := git.RunHeadRef(run.ID)
	runRefHead, runRefExists, runRefErr := optionalExactRef(ctx, gateDir, runRef)
	if runRefErr != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the exact run-owned preservation ref could not be verified; no files or branch refs were changed")
	}
	if runRefExists && runRefHead != preserved {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_run_head_mismatch", fmt.Sprintf("the run-owned preservation ref names %s, not recorded head %s; both refs were retained for diagnosis", runRefHead, preserved))
	}

	anchored := false
	if existing, anchorErr := git.ResolveRef(ctx, wd, anchorRef); anchorErr == nil && existing == preserved {
		anchored = true
	}
	legacyMismatch := false
	adoptContainedCurrentHead := false
	switch {
	case gateHead == preserved:
		// The exact run-owned ref is byte-verified against durable authority
		// above; pin it when a legacy row never had one.
		if !runRefExists {
			if err := git.PinRunHead(ctx, gateDir, run.ID, preserved); err != nil {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the preserved pipeline head could not be pinned under its exact run-owned gate ref; no custody was returned")
			}
			runRefExists = true
		}
	case s.legacySubmittedMismatchEligible(ctx, run, state, gateHead):
		// Historical rebase failures recorded R while both the clean operator
		// and mutable gate branch remained at immutable submitted A. Accepting
		// an already-pinned run ref here is what makes a repeated recovery
		// idempotent; it never authorizes recovery across an unexplained third
		// branch head, which stays with the refusal below whether or not a run
		// ref exists.
		if !runRefExists {
			if _, err := git.ResolveExactCommit(ctx, gateDir, preserved); err != nil {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the historical recorded pipeline commit is missing from the managed gate object store; no files, refs, or custody state were changed")
			}
			if err := git.PinRunHead(ctx, gateDir, run.ID, preserved); err != nil {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the historical recorded pipeline commit could not be pinned exactly; no custody was returned")
			}
			runRefExists = true
		}
		legacyMismatch = true
	case keepLocal && runRefExists:
		// The operator may have already pushed a rebased copy of the preserved
		// changes before returning custody. That gate head is explainable only
		// after the exact run-owned ref is fetched and every preserved commit is
		// proven present in the current head by patch content, not ancestry.
		adoptContainedCurrentHead = true
	default:
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_diverged", fmt.Sprintf("the gate branch is at %s, which is neither the preserved pipeline head %s recorded for this run nor its immutable submitted head; that live third head is never displaced automatically, so no files or refs were changed", gateHead, preserved))
	}

	if !anchored {
		if fetchErr := git.FetchRemoteRefToPrivateRef(ctx, wd, gateDir, runRef, anchorRef); fetchErr != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the exact run-owned pipeline head could not be fetched from the local gate; no custody was returned")
		}
		if fetched, fetchErr := git.ResolveRef(ctx, wd, anchorRef); fetchErr != nil || fetched != preserved {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the fetched recovery ref did not byte-match the recorded pipeline head; no custody was returned")
		}
		anchored = true
	}
	if legacyMismatch {
		if s.beforeLegacyRecoveryRecheck != nil {
			s.beforeLegacyRecoveryRecheck()
		}
		if !s.legacySubmittedMismatchStillExact(ctx, run, state, gateHead) {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch, clean worktree, gate branch, run authority, or active owner changed while the historical head was being preserved; both preservation refs were retained and custody was not returned")
		}
		if keepLocal {
			// A is already both the local and gate submitted head. Preserve R
			// under run/recovery refs and leave files plus both mutable heads at A.
			return s.finishRecover(ctx, run, false)
		}
	} else if !s.recoveryAssumptionsStillExact(ctx, run, state, gateHead) {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch, worktree, gate branch, run authority, or active owner changed while the exact head was being anchored; preservation refs were retained and custody was not returned")
	}
	if adoptContainedCurrentHead {
		missing, err := missingPreservedCommitsByPatch(ctx, wd, ptr(run.SubmittedHeadSHA), preserved, local)
		if err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", fmt.Sprintf("the preserved commits could not be compared with the current head by patch content (%v); no files, refs, or custody state were changed", err))
		}
		if len(missing) > 0 {
			blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_missing_preserved_commits", missingPreservedCommitsMessage(ctx, wd, missing, anchorRef))
			blocked.NextAction = &NextAction{Code: "apply_missing_preserved_commits", Command: "git cherry-pick " + strings.Join(missing, " ")}
			return blocked
		}
		return s.recoverAdoptedCurrentHead(ctx, run, state, gateHead, preserved)
	}

	switch {
	case local == preserved, isAncestor(ctx, wd, preserved, local):
		// Equal or ahead, discovered only after anchoring made the preserved
		// head comparable locally.
		return s.finishRecover(ctx, run, false)
	case isAncestor(ctx, wd, local, preserved):
		if keepLocal {
			return s.recoverKeepLocal(ctx, run, state, gateHead)
		}
		if !state.Local.Clean {
			state.Relation = RelationBehind
			blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_dirty", fmt.Sprintf("the invoking worktree is not clean (%s); commit or stash first and re-run the recovery, or use --keep-local to return custody at the current head without moving the worktree; no files or refs were changed", state.Local.Reason))
			blocked.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
			return blocked
		}
		return s.recoverFastForward(ctx, run, state, preserved)
	default:
		if keepLocal {
			return s.recoverKeepLocal(ctx, run, state, gateHead)
		}
		state.Relation = RelationDiverged
		// Only offer rerun when it would actually run: it resumes the preserved
		// head exclusively while the mutable gate branch still equals it, so on
		// a legacy or moved-branch shape that advice is a dead end.
		options := "reconcile manually and re-run the recovery"
		if gateHead == preserved {
			options += ", run `no-mistakes rerun` to resume validating the preserved head"
		}
		options += ", or use --keep-local to keep the current head"
		blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_diverged", fmt.Sprintf("the local branch and the preserved pipeline head have diverged; the preserved commits are anchored at %s - %s; no files or refs were changed", anchorRef, options))
		blocked.NextAction = &NextAction{Code: "inspect_and_reconcile_manually", Command: "git log --oneline --left-right HEAD..." + anchorRef}
		return blocked
	}
}

// ResolveAmbiguousHead is the explicit, non-destructive exit from
// blocked_pipeline_owned_ambiguous. Preservation evidence names more than one
// head and the tool must never choose, so the operator names the exact commit
// that becomes run authority. Every other candidate is archived under an
// immutable per-run archive ref FIRST, then the run-owned ref is
// compare-and-swapped to the named commit, then durable authority follows with
// its own compare-and-swap, and only then is the crash candidate retired. Any
// losing race stops with every candidate still archived, so a partial
// resolution is re-runnable rather than lossy.
//
// Resolution deliberately touches no mutable branch, worktree file, or custody
// stamp: it only restores agreement between Git evidence and durable authority,
// after which the branch is ordinarily recoverable through `--recover`.
func (s *Service) ResolveAmbiguousHead(ctx context.Context, chosen string) State {
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return refusal
	}
	state, run, _ := s.inspect(ctx)
	if PreservationUnreadable(state) {
		return blockedPlan(state, StateAmbiguousContext, "blocked_resolve_preservation_unreadable", preservationUnreadableMessage)
	}
	if run == nil || state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned_ambiguous" {
		return blockedPlan(state, state.State, "blocked_resolve_not_ambiguous", "this branch has no ambiguous preserved head to resolve; no files, refs, or custody state were changed")
	}
	gateDir := strings.TrimSpace(s.GateDir)
	if gateDir == "" {
		return blockedPlan(state, StatePipelineOwned, "blocked_resolve_gate_unavailable", "no local gate is configured for this repository, so the preserved heads cannot be verified; no files or refs were changed")
	}
	if active, err := s.DB.GetActiveRun(run.RepoID, state.Local.Branch); err != nil || active != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_resolve_run_active", "a run is active on this branch; drive it to completion or abort it before resolving preserved-head ambiguity; no files or refs were changed")
	}

	refs, refsErr := s.loadPreservedHeads(ctx)
	if refsErr != nil {
		return blockedPlan(state, StateAmbiguousContext, "blocked_resolve_preservation_unreadable", preservationUnreadableMessage)
	}
	runRef, crashRef := git.RunHeadRef(run.ID), git.CrashHeadRef(run.ID)
	recorded := strings.TrimSpace(run.HeadSHA)
	runRefHead, runRefExists := refs[runRef]
	crashHead, crashExists := refs[crashRef]
	candidates := candidateHeads(refs, run)

	chosen = strings.TrimSpace(chosen)
	if !containsHead(candidates, chosen) {
		return blockedPlan(state, StatePipelineOwned, "blocked_resolve_head_not_candidate", fmt.Sprintf("the resolution must name one of this run's exact preserved candidate heads (%s); no files, refs, or custody state were changed", strings.Join(candidates, ", ")))
	}
	if _, err := git.ResolveExactCommit(ctx, gateDir, chosen); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_resolve_head_unverifiable", fmt.Sprintf("the named commit %s does not resolve exactly in the managed gate object store; no files, refs, or custody state were changed", chosen))
	}
	for _, losing := range candidates {
		if losing == chosen {
			continue
		}
		if err := git.PinExactCommit(ctx, gateDir, git.ArchiveHeadRef(run.ID, losing), losing); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_resolve_archive_failed", fmt.Sprintf("the losing head %s could not be archived, so nothing was retired; no files, refs, or custody state were changed", losing))
		}
	}
	if !runRefExists || runRefHead != chosen {
		expectedOld := ""
		if runRefExists {
			expectedOld = runRefHead
		}
		if err := git.CompareAndSwapPrivateRef(ctx, gateDir, runRef, chosen, expectedOld); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_resolve_race", "the run-owned preservation ref changed while the resolution was being published; every candidate head stays archived and nothing was retired")
		}
	}
	if recorded != chosen {
		moved, err := s.DB.ResolveRunHeadSHA(run.ID, recorded, chosen)
		if err != nil || !moved {
			return blockedPlan(state, StatePipelineOwned, "blocked_resolve_race", "durable run authority changed while the resolution was being published; every candidate head stays archived and nothing was retired")
		}
	}
	if crashExists {
		if err := git.DeletePrivateRefExactly(ctx, gateDir, crashRef, crashHead); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_resolve_retire_failed", "the crash-candidate ref could not be retired after the resolution was published; every candidate head stays archived and the branch stays blocked")
		}
	}
	resolved, _, _ := s.inspect(ctx)
	resolved.Changed = true
	return resolved
}

func optionalExactRef(ctx context.Context, repoDir, ref string) (string, bool, error) {
	exists, err := git.RefExists(ctx, repoDir, ref)
	if err != nil || !exists {
		return "", false, err
	}
	resolved, err := git.ResolveRef(ctx, repoDir, ref)
	if err != nil {
		return "", false, err
	}
	return resolved, true, nil
}

func (s *Service) legacySubmittedMismatchEligible(ctx context.Context, run *db.Run, state State, gateHead string) bool {
	if run.SubmittedHeadSHA == nil || strings.TrimSpace(*run.SubmittedHeadSHA) == "" || run.HeadSHA == *run.SubmittedHeadSHA {
		return false
	}
	if !state.Local.Clean || state.Local.Head != *run.SubmittedHeadSHA || gateHead != *run.SubmittedHeadSHA {
		return false
	}
	active, err := s.DB.GetActiveRun(run.RepoID, state.Local.Branch)
	return err == nil && (active == nil || active.ID == run.ID)
}

func (s *Service) localRecoveryAssumptionsStillExact(ctx context.Context, run *db.Run, state State) bool {
	current, err := s.DB.GetRun(run.ID)
	if err != nil || current == nil || !terminalRunStatus(current.Status) || current.CustodyReturnedAt != nil || current.HeadSHA != run.HeadSHA {
		return false
	}
	branch, err := git.CurrentBranch(ctx, s.workDir())
	if err != nil || branch != state.Local.Branch {
		return false
	}
	head, err := git.HeadSHA(ctx, s.workDir())
	if err != nil || head != state.Local.Head {
		return false
	}
	clean, reason := worktreeClean(ctx, s.workDir())
	if clean != state.Local.Clean || reason != state.Local.Reason {
		return false
	}
	active, err := s.DB.GetActiveRun(run.RepoID, state.Local.Branch)
	return err == nil && active == nil
}

func (s *Service) recoveryAssumptionsStillExact(ctx context.Context, run *db.Run, state State, expectedGate string) bool {
	current, err := s.DB.GetRun(run.ID)
	if err != nil || current == nil || !terminalRunStatus(current.Status) || current.CustodyReturnedAt != nil || current.HeadSHA != run.HeadSHA {
		return false
	}
	branch, err := git.CurrentBranch(ctx, s.workDir())
	if err != nil || branch != state.Local.Branch {
		return false
	}
	head, err := git.HeadSHA(ctx, s.workDir())
	if err != nil || head != state.Local.Head {
		return false
	}
	clean, reason := worktreeClean(ctx, s.workDir())
	if clean != state.Local.Clean || reason != state.Local.Reason {
		return false
	}
	gateHead, err := git.ResolveRef(ctx, s.GateDir, "refs/heads/"+state.Local.Branch)
	if err != nil || gateHead != expectedGate {
		return false
	}
	active, err := s.DB.GetActiveRun(run.RepoID, state.Local.Branch)
	return err == nil && active == nil
}

// legacySubmittedMismatchStillExact re-reads every mutable input after the
// historical object has been pinned and fetched. A racing local commit, gate
// push, DB update, worktree dirtiness change, or newer active owner wins and
// leaves custody unstamped.
func (s *Service) legacySubmittedMismatchStillExact(ctx context.Context, run *db.Run, state State, expectedGate string) bool {
	current, err := s.DB.GetRun(run.ID)
	if err != nil || current == nil || !terminalRunStatus(current.Status) || current.CustodyReturnedAt != nil || current.HeadSHA != run.HeadSHA || current.SubmittedHeadSHA == nil || run.SubmittedHeadSHA == nil || *current.SubmittedHeadSHA != *run.SubmittedHeadSHA {
		return false
	}
	branch, err := git.CurrentBranch(ctx, s.workDir())
	if err != nil || branch != state.Local.Branch {
		return false
	}
	head, err := git.HeadSHA(ctx, s.workDir())
	if err != nil || head != state.Local.Head || head != *run.SubmittedHeadSHA {
		return false
	}
	clean, _ := worktreeClean(ctx, s.workDir())
	if !clean {
		return false
	}
	gateHead, err := git.ResolveRef(ctx, s.GateDir, "refs/heads/"+state.Local.Branch)
	if err != nil || gateHead != expectedGate || gateHead != *run.SubmittedHeadSHA {
		return false
	}
	active, err := s.DB.GetActiveRun(run.RepoID, state.Local.Branch)
	return err == nil && (active == nil || active.ID == run.ID)
}

// recoverKeepLocal performs the explicit keep-local custody return: the
// worktree is never touched; the gate branch moves to the kept local head with
// an atomic compare-and-swap so a concurrent gate push refuses instead of
// being clobbered. The kept head's objects reach the gate through a gate-side
// fetch - never a push, which would fire the gate's receive hooks and start a
// pipeline run. The preserved head stays reachable through the anchor ref.
func (s *Service) recoverKeepLocal(ctx context.Context, run *db.Run, state State, gateHead string) State {
	if blocked, ok := s.ensureGateAtLocalHead(ctx, run, state, gateHead); !ok {
		return blocked
	}
	return s.finishRecover(ctx, run, false)
}

func (s *Service) recoverAdoptedCurrentHead(ctx context.Context, run *db.Run, state State, gateHead, preserved string) State {
	if err := git.PinExactCommit(ctx, s.GateDir, recoverGateArchiveRef(run.ID, gateHead), gateHead); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_archive_failed", fmt.Sprintf("the diverged gate head %s could not be archived before custody return; no files, refs, or custody state were changed", gateHead))
	}
	if err := git.PinExactCommit(ctx, s.GateDir, recoverPreservedArchiveRef(run.ID, preserved), preserved); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_archive_failed", fmt.Sprintf("the original preserved head %s could not be archived before current-head adoption; no files, refs, or custody state were changed", preserved))
	}
	if !s.recoveryAssumptionsStillExact(ctx, run, state, gateHead) {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch, worktree, gate branch, run authority, or active owner changed while the contained current head was being adopted; the preservation refs were retained and custody was not returned")
	}
	if blocked, ok := s.ensureGateAtLocalHead(ctx, run, state, gateHead); !ok {
		return blocked
	}
	if err := git.CompareAndSwapPrivateRef(ctx, s.GateDir, git.RunHeadRef(run.ID), state.Local.Head, preserved); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_run_head_race", "the run-owned preservation ref changed while the contained current head was being adopted; the recovery archives were retained and custody was not returned")
	}
	adopted, err := s.DB.AdoptRunHeadForCustodyReturn(run.ID, preserved, state.Local.Head)
	if err != nil || !adopted {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_stamp_failed", "the contained current head could not be recorded as recovered run authority; re-run the recovery")
	}
	adoptedState, _, _ := s.inspect(ctx)
	adoptedState.Recovered = true
	adoptedState.Changed = false
	return adoptedState
}

func (s *Service) ensureGateAtLocalHead(ctx context.Context, run *db.Run, state State, gateHead string) (State, bool) {
	if s.beforeGateReset != nil {
		s.beforeGateReset()
	}
	head, err := git.HeadSHA(ctx, s.workDir())
	if err != nil || head != state.Local.Head {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch head changed while custody was being returned; no files or refs were changed"), false
	}
	if gateHead == state.Local.Head {
		return State{}, true
	}
	// The fetch source must be absolute: the command runs inside the gate
	// directory, where a relative invoking-worktree path would resolve to
	// the gate itself.
	source, err := filepath.Abs(s.workDir())
	if err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the invoking worktree path could not be resolved; no files or refs were changed"), false
	}
	stagingRef := "refs/no-mistakes/custody-return/" + run.ID
	if _, err := git.Run(ctx, s.GateDir, "fetch", "--no-tags", "--no-write-fetch-head", source, "+refs/heads/"+state.Local.Branch+":"+stagingRef); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the kept local head could not be staged into the gate; no files or refs were changed"), false
	}
	staged, err := git.Run(ctx, s.GateDir, "rev-parse", stagingRef+"^{commit}")
	if err != nil || staged != state.Local.Head {
		_, _ = git.Run(ctx, s.GateDir, "update-ref", "-d", stagingRef)
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch head changed while custody was being returned; no files or refs were changed"), false
	}
	_, casErr := git.Run(ctx, s.GateDir, "update-ref", "refs/heads/"+state.Local.Branch, state.Local.Head, gateHead)
	_, _ = git.Run(ctx, s.GateDir, "update-ref", "-d", stagingRef)
	if casErr != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the gate branch changed while custody was being returned; re-run the recovery; no local files or refs were changed"), false
	}
	return State{}, true
}

// recoverFastForward advances the clean checked-out branch to the preserved
// pipeline head with the same strict fast-forward and honesty rules as Apply.
func (s *Service) recoverFastForward(ctx context.Context, run *db.Run, state State, preserved string) State {
	if s.beforeRecoverFastForward != nil {
		s.beforeRecoverFastForward()
	}
	branch, branchErr := git.CurrentBranch(ctx, s.workDir())
	head, headErr := git.HeadSHA(ctx, s.workDir())
	clean, _ := worktreeClean(ctx, s.workDir())
	if branchErr != nil || branch != state.Local.Branch || headErr != nil || head != state.Local.Head || !clean {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch or worktree changed while custody was being returned; no files or refs were changed")
	}
	_, mergeErr := git.Run(ctx, s.workDir(), "merge", "--ff-only", "--no-edit", preserved)
	finalHead, _ := git.HeadSHA(ctx, s.workDir())
	finalClean, finalReason := worktreeClean(ctx, s.workDir())
	state.Local.Head = finalHead
	state.Local.Clean = finalClean
	state.Local.Reason = finalReason
	state.Changed = finalHead == preserved && finalHead != head
	if mergeErr != nil || finalHead != preserved {
		blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_apply_failed", fmt.Sprintf("strict fast-forward to the preserved pipeline head failed; final HEAD is %s and no destructive recovery was attempted", finalHead))
		return blocked
	}
	if !finalClean {
		state.State = StateDirty
		state.Relation = RelationEqual
		state.Safety = "blocked_post_recover_" + finalReason
		state.Error = "HEAD reached the preserved pipeline head, but a Git hook left the worktree non-clean; custody was not recorded"
		state.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
		return state
	}
	return s.finishRecover(ctx, run, true)
}

func (s *Service) anchorReachablePreserved(ctx context.Context, state State, anchorRef, preserved string) (State, bool) {
	if _, err := git.Run(ctx, s.workDir(), "update-ref", anchorRef, preserved); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the preserved pipeline commits could not be anchored locally; no files or refs were changed"), false
	}
	if anchored, err := git.Run(ctx, s.workDir(), "rev-parse", anchorRef+"^{commit}"); err != nil || anchored != preserved {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the preserved pipeline commits could not be anchored locally; no files or refs were changed"), false
	}
	return State{}, true
}

// finishRecover stamps custody returned and reports the fresh post-recovery
// truth. changed reports whether this call moved the worktree HEAD.
func (s *Service) finishRecover(ctx context.Context, run *db.Run, changed bool) State {
	if err := s.DB.SetRunCustodyReturned(run.ID); err != nil {
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Safety = "blocked_recover_stamp_failed"
		state.Error = "the custody return could not be recorded; re-run the recovery"
		state.NextAction = nil
		return state
	}
	state, _, _ := s.inspect(ctx)
	state.Recovered = true
	state.Changed = changed
	return state
}

func recoverAnchorRef(runID string) string {
	return "refs/no-mistakes/recover/" + runID
}

func recoverGateArchiveRef(runID, sha string) string {
	return "refs/no-mistakes/recover/" + runID + "-gate-" + sha
}

func recoverPreservedArchiveRef(runID, sha string) string {
	return "refs/no-mistakes/recover/" + runID + "-preserved-" + sha
}

func missingPreservedCommitsByPatch(ctx context.Context, dir, submitted, preserved, current string) ([]string, error) {
	submitted = strings.TrimSpace(submitted)
	preserved = strings.TrimSpace(preserved)
	current = strings.TrimSpace(current)
	if submitted == "" || preserved == "" || current == "" {
		return nil, fmt.Errorf("submitted, preserved, and current heads are required")
	}
	for _, sha := range []string{submitted, preserved, current} {
		if _, err := git.ResolveExactCommit(ctx, dir, sha); err != nil {
			return nil, err
		}
	}
	out, err := git.Run(ctx, dir, "cherry", current, preserved, submitted)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 2 || (fields[0] != "+" && fields[0] != "-") {
			return nil, fmt.Errorf("unexpected git cherry output %q", line)
		}
		if fields[0] == "+" {
			missing = append(missing, fields[1])
		}
	}
	return missing, nil
}

func missingPreservedCommitsMessage(ctx context.Context, dir string, missing []string, anchorRef string) string {
	var described []string
	for _, sha := range missing {
		if line, err := git.Run(ctx, dir, "show", "-s", "--format=%H %s", sha); err == nil && strings.TrimSpace(line) != "" {
			described = append(described, strings.TrimSpace(line))
		} else {
			described = append(described, sha)
		}
	}
	return fmt.Sprintf("the current head does not contain every preserved pipeline commit by patch content; missing preserved commits: %s; the preserved head is anchored at %s. Apply or cherry-pick the missing commits, then re-run `no-mistakes axi sync --recover --keep-local`, or manually move the branch back to the preserved head and run `no-mistakes axi sync --recover`; no files, refs, or custody state were changed", strings.Join(described, ", "), anchorRef)
}

func (s *Service) inspect(ctx context.Context) (State, *db.Run, bool) {
	state := State{Relation: RelationUnknown, Safety: "blocked_ambiguous_context", Remote: RemoteState{Freshness: "unknown"}}
	root, err := git.FindGitRoot(s.workDir())
	if err != nil {
		state.State = StateAmbiguousContext
		state.Error = "the invoking directory is not a registered Git worktree"
		return state, nil, false
	}
	mainRoot, err := git.FindMainRepoRoot(root)
	if err != nil || !samePath(mainRoot, s.Repo.WorkingPath) {
		state.State = StateAmbiguousContext
		state.Error = "the invoking worktree does not belong to the registered repository"
		return state, nil, false
	}
	branch, err := git.CurrentBranch(ctx, root)
	if err != nil || branch == "" || branch == "HEAD" {
		state.State = StateAmbiguousContext
		state.Error = "synchronization requires an exact checked-out branch, not detached HEAD"
		return state, nil, false
	}
	head, err := git.HeadSHA(ctx, root)
	if err != nil {
		state.State = StateAmbiguousContext
		state.Error = "could not resolve the invoking worktree HEAD"
		return state, nil, false
	}
	state.Local = LocalState{Branch: branch, Head: head}
	clean, reason := worktreeClean(ctx, root)
	state.Local.Clean = clean
	state.Local.Reason = reason

	runs, err := s.DB.GetRunsByRepo(s.Repo.ID)
	if err != nil {
		state.State = StateAmbiguousContext
		state.Error = "could not load pipeline push provenance"
		return state, nil, false
	}
	// The snapshot is an input to run selection as well as to the ambiguity
	// classification, so an unreadable refs backend makes every downstream
	// answer unreliable. Reporting one anyway would be the same fail-open in a
	// different disguise.
	refs, refsErr := s.loadPreservedHeads(ctx)
	if refsErr != nil {
		state.State = StateAmbiguousContext
		state.Safety = SafetyPreservationUnreadable
		state.Error = preservationUnreadableMessage
		state.NextAction = &NextAction{Code: "inspect_gate_preservation", Command: "no-mistakes doctor"}
		return state, nil, false
	}
	var run *db.Run
	for _, candidate := range runs {
		if candidate.Branch != branch {
			continue
		}
		if candidate.Status == types.RunPending || candidate.Status == types.RunRunning || unpublishedPipelineHead(candidate) {
			run = candidate
			break
		}
		if _, ambiguous := ambiguousPreservedHead(refs, candidate); ambiguous {
			run = candidate
			break
		}
		// Custody-returned runs stay selectable so a recovered branch reports
		// custody_returned (or its ordinary post-push classification) instead
		// of falling back to an older binding or an ambiguous no-match.
		if run == nil && (candidate.LastPushedSHA != nil || candidate.CustodyReturnedAt != nil) {
			run = candidate
		}
	}
	if run == nil {
		if len(runs) > 0 {
			state.State = StateAmbiguousContext
			state.Safety = "blocked_wrong_branch"
			state.Error = "the checked-out branch does not match any pipeline push binding"
		} else {
			state.State = StateLegacyUnbound
			state.Safety = "blocked_legacy_unbound"
			state.Error = "no exact successful pipeline push binding exists for the checked-out branch"
		}
		return state, nil, false
	}

	state.Pipeline = PipelineState{
		RunID: run.ID, Status: string(run.Status), SubmittedHead: ptr(run.SubmittedHeadSHA), CurrentHead: run.HeadSHA,
		PushedHead: ptr(run.LastPushedSHA), PushedAt: value(run.LastPushedAt), PushGeneration: value(run.PushGeneration),
	}
	state.PRState = normalizePRState(run.PRState)
	state.Target = TargetState{Kind: ptr(run.PushTargetKind), URL: displayTarget(s.Repo.PushURL()), Ref: ptr(run.PushRef)}
	state.Target.Remote = s.remoteName(ctx)
	state.Remote = RemoteState{ObservedHead: ptr(run.LastPushedSHA), Freshness: "pipeline_push", ObservedAt: value(run.LastPushedAt)}

	if run.PushActive || pushStepRunning(s.DB, run.ID) {
		state.State = StatePushInProgress
		state.Safety = "blocked_push_in_progress"
		state.Pipeline.Phase = "push"
		state.NextAction = &NextAction{Code: "continue_active_run", Command: "no-mistakes axi status"}
		return state, run, false
	}
	if classifyAmbiguousPreservedHead(refs, &state, run) {
		return state, run, false
	}
	if run.LastPushedSHA == nil || run.PushTargetFingerprint == nil || run.PushRef == nil || run.PushGeneration == nil || run.SubmittedHeadSHA == nil {
		if run.SubmittedHeadSHA != nil && run.HeadSHA != ptr(run.SubmittedHeadSHA) {
			if run.CustodyReturnedAt != nil {
				s.classifyCustodyReturned(ctx, &state)
				return state, run, true
			}
			s.classifyPipelineOwned(ctx, &state, run, "the pipeline head has moved but has not been successfully pushed; do not make local follow-up commits yet")
			return state, run, false
		}
		state.State = StateLegacyUnbound
		state.Safety = "blocked_legacy_unbound"
		state.Error = "this run has no exact successful push provenance and cannot be synchronized safely"
		return state, run, false
	}
	if run.HeadSHA != ptr(run.LastPushedSHA) && run.CustodyReturnedAt == nil {
		s.classifyPipelineOwned(ctx, &state, run, "the pipeline head has not been successfully bound to the push target; do not make local follow-up commits yet")
		return state, run, false
	}
	// Terminal PR lifecycle retires the branch regardless of local dirtiness
	// or later target configuration. Refresh may classify retained versus
	// removed only while the exact original target binding still matches.
	if state.PRState == "merged" {
		state.State = StateMergedRemoteRetained
		state.Safety = "blocked_merged"
		return state, run, true
	}
	if state.PRState == "closed" {
		state.State = StateClosed
		state.Safety = "blocked_closed"
		return state, run, true
	}
	if ptr(run.PushRef) != "refs/heads/"+branch || ptr(run.PushTargetFingerprint) != TargetFingerprint(s.Repo.PushURL()) || ptr(run.PushTargetKind) != targetKind(s.Repo) {
		state.State = StateTargetChanged
		state.Safety = "blocked_target_changed"
		state.Error = "the configured push target or branch ref changed after the pipeline push"
		return state, run, false
	}
	if duplicateBranchCheckout(ctx, root, branch) {
		state.State = StateAmbiguousContext
		state.Safety = "blocked_branch_ambiguous"
		state.Error = "the checked-out branch is attached to more than one worktree"
		return state, run, false
	}
	if !clean {
		state.State = StateDirty
		state.Safety = "blocked_" + reason
		state.Error = "the invoking worktree is not completely clean; no network read or mutation was attempted"
		state.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
		return state, run, false
	}

	s.classifyRelation(ctx, &state, ptr(run.LastPushedSHA), run.BaseSHA, false)
	return state, run, true
}

func (s *Service) classifyRelation(ctx context.Context, state *State, pushed, base string, live bool) {
	if state.Local.Head == pushed {
		state.State = StateSynchronized
		state.Relation = RelationEqual
		state.Safety = "already_synchronized"
		state.NextAction = nil
		return
	}
	if objectExists(ctx, s.workDir(), pushed) {
		switch {
		case isAncestor(ctx, s.workDir(), state.Local.Head, pushed):
			state.State = StateBehind
			state.Relation = RelationBehind
		case isAncestor(ctx, s.workDir(), pushed, state.Local.Head):
			state.State = StateLocalAhead
			state.Relation = RelationAhead
			state.Safety = "blocked_local_ahead"
			state.NextAction = &NextAction{Code: "run_pipeline", Command: `no-mistakes axi run --intent "<what the user set out to accomplish>"`}
			return
		default:
			if equivalentDivergence(ctx, s.workDir(), state.Local.Head, pushed, base) {
				state.State = StateDiverged
				state.Relation = RelationDiverged
				if live {
					state.Safety = SafetySafeEquivalentAdvance
				} else {
					state.Safety = "refresh_required"
				}
				state.NextAction = &NextAction{Code: "sync", Command: "no-mistakes axi sync"}
				state.Error = ""
				return
			}
			state.State = StateDiverged
			state.Relation = RelationDiverged
			state.Safety = "blocked_diverged"
			state.NextAction = &NextAction{Code: "inspect_and_reconcile_manually", Command: "git log --oneline --left-right HEAD..." + pushed}
			state.Error = "local and pipeline-pushed histories have diverged; no files or refs were changed"
			return
		}
	} else if state.Local.Head == state.Pipeline.SubmittedHead && state.Pipeline.SubmittedHead != pushed {
		state.State = StateBehind
		state.Relation = RelationBehind
	} else {
		state.State = StateAmbiguousContext
		state.Relation = RelationUnknown
		state.Safety = "blocked_relation_unknown"
		state.Error = "the pipeline-pushed commit is not available locally; run an explicit synchronization check"
		state.NextAction = &NextAction{Code: "check_sync", Command: "no-mistakes sync --check"}
		return
	}
	if live {
		state.Safety = SafetySafeFastForward
	} else {
		state.Safety = "refresh_required"
	}
	state.NextAction = &NextAction{Code: "sync", Command: "no-mistakes axi sync"}
}

func syncAnchorRef(runID string) string {
	return "refs/no-mistakes/sync-anchor/" + runID
}

func equivalentDivergence(ctx context.Context, dir, local, pushed, base string) bool {
	if local == "" || pushed == "" || local == pushed {
		return false
	}
	base = usableEquivalenceBase(ctx, dir, local, pushed, base)
	if base == "" {
		return false
	}
	_, err := revList(ctx, dir, append([]string{"rev-list", "--right-only", pushed + "..." + local}, "^"+base)...)
	if err != nil {
		return false
	}
	return mergeTreePreservesFinalHead(ctx, dir, base, local, pushed)
}

func usableEquivalenceBase(ctx context.Context, dir, local, pushed, base string) string {
	if base != "" && !git.IsZeroSHA(base) && objectExists(ctx, dir, base) {
		return base
	}
	mergeBase, err := git.Run(ctx, dir, "merge-base", local, pushed)
	if err != nil {
		return ""
	}
	return mergeBase
}

func revList(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, err := git.Run(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	return strings.Fields(out), nil
}

func mergeTreePreservesFinalHead(ctx context.Context, dir, base, local, pushed string) bool {
	mergedTree, err := git.Run(ctx, dir, "merge-tree", "--write-tree", "--merge-base", base, pushed, local)
	if err != nil {
		return false
	}
	pushedTree, err := git.Run(ctx, dir, "rev-parse", pushed+"^{tree}")
	if err != nil {
		return false
	}
	return mergedTree == pushedTree
}

func (s *Service) remoteName(ctx context.Context) string {
	out, err := git.Run(ctx, s.workDir(), "remote")
	if err == nil {
		for _, name := range strings.Fields(out) {
			remoteURL, err := git.GetConfiguredRemoteURL(ctx, s.workDir(), name)
			if err == nil && TargetFingerprint(remoteURL) == TargetFingerprint(s.Repo.PushURL()) {
				return name
			}
		}
	}
	if strings.TrimSpace(s.Repo.ForkURL) != "" {
		return "fork"
	}
	return "origin"
}

func (s *Service) workDir() string {
	if strings.TrimSpace(s.WorkDir) == "" {
		return "."
	}
	return s.WorkDir
}

func refreshable(state State) bool {
	switch state.State {
	case StateBehind, StateSynchronized, StateLocalAhead, StateDiverged, StateMergedRemoteRetained, StateClosed, StateAmbiguousContext:
		return true
	default:
		return false
	}
}

func worktreeClean(ctx context.Context, dir string) (bool, string) {
	markers := []struct{ path, reason string }{
		{"MERGE_HEAD", "merge_in_progress"}, {"rebase-merge", "rebase_in_progress"}, {"rebase-apply", "rebase_in_progress"},
		{"CHERRY_PICK_HEAD", "cherry_pick_in_progress"}, {"REVERT_HEAD", "revert_in_progress"}, {"BISECT_LOG", "bisect_in_progress"}, {"sequencer", "sequencer_in_progress"},
	}
	for _, marker := range markers {
		path, err := git.Run(ctx, dir, "rev-parse", "--git-path", marker.path)
		if err == nil {
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			if _, err := os.Stat(path); err == nil {
				return false, marker.reason
			}
		}
	}
	dirty, err := git.HasUncommittedChanges(ctx, dir)
	if err != nil {
		return false, "status_unavailable"
	}
	if dirty {
		return false, "dirty"
	}
	return true, ""
}

func duplicateBranchCheckout(ctx context.Context, dir, branch string) bool {
	out, err := git.Run(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return true
	}
	needle := "branch refs/heads/" + branch
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if line == needle {
			count++
		}
	}
	return count != 1
}

func unpublishedPipelineHead(run *db.Run) bool {
	if run == nil || run.SubmittedHeadSHA == nil || run.CustodyReturnedAt != nil {
		return false
	}
	if run.LastPushedSHA == nil {
		return run.HeadSHA != ptr(run.SubmittedHeadSHA)
	}
	return run.HeadSHA != ptr(run.LastPushedSHA)
}

func terminalRunStatus(status types.RunStatus) bool {
	switch status {
	case types.RunCompleted, types.RunFailed, types.RunCancelled:
		return true
	default:
		return false
	}
}

// classifyPipelineOwned reports an unpublished pipeline head. While the run is
// active the block is absolute: the pipeline will publish or keep moving the
// head, so the worktree must wait. Once the run is TERMINAL nothing will ever
// publish that head - the branch would be stranded in custody forever - so the
// same state becomes recoverable and points at the guarded custody-return
// action (issue: v1.38.1 dogfood, cancelled pre-push run).
func (s *Service) classifyPipelineOwned(_ context.Context, state *State, run *db.Run, activeMessage string) {
	state.State = StatePipelineOwned
	state.Pipeline.Phase = "pre_push"
	if terminalRunStatus(run.Status) {
		state.Safety = "blocked_pipeline_owned_recoverable"
		state.Error = "the run finished " + string(run.Status) + " with unpublished pipeline commits preserved in the local gate; recover custody before any local follow-up commit"
		state.NextAction = &NextAction{Code: "recover_custody", Command: "no-mistakes axi sync --recover"}
		return
	}
	state.Safety = "blocked_pipeline_owned"
	state.Error = activeMessage
	state.NextAction = &NextAction{Code: "continue_active_run", Command: "no-mistakes axi status"}
}

// SafetyPreservationUnreadable marks the one direction this guard may not fail
// in: the gate's preservation refs could not be read, so no surface may claim a
// branch is free of preserved-head ambiguity. It is branch-scoped rather than
// run-scoped, because the unreadable snapshot is an input to run selection
// itself - no run can be named while it holds. Presenters must render it and
// fresh-run preflight must refuse on it; see PreservationUnreadable.
const SafetyPreservationUnreadable = "blocked_preservation_unreadable"

const preservationUnreadableMessage = "the managed gate's preservation refs could not be read, so preserved-head ambiguity cannot be ruled out for this branch; every head is retained and no files, refs, or custody state were changed"

// PreservationUnreadable reports the branch-scoped unreadable-evidence block.
// Every surface that filters or renders branch-sync state must consult it, so
// the refusal cannot become invisible by falling through a state-only switch.
func PreservationUnreadable(state State) bool {
	return state.State == StateAmbiguousContext && state.Safety == SafetyPreservationUnreadable
}

// preservedHeads is one snapshot of every run-owned and crash preservation ref
// in the gate, keyed by full ref name. It is read once per inspection: probing
// per candidate run cost up to three Git subprocesses each and grew linearly
// with never-pruned run history on every status, home, and TUI refresh.
type preservedHeads map[string]string

// loadPreservedHeads separates "there is no managed gate to read" from "the
// gate's refs could not be read". Only the first is an empty snapshot: an
// unreadable refs backend must never collapse into "no ambiguity", because that
// is the one direction in which this guard may not fail. A missing or
// unconfigured gate is owned by the downstream gate-unavailable refusals.
func (s *Service) loadPreservedHeads(ctx context.Context) (preservedHeads, error) {
	gateDir := strings.TrimSpace(s.GateDir)
	if gateDir == "" {
		return nil, nil
	}
	if _, err := os.Stat(gateDir); err != nil {
		return nil, nil
	}
	return git.PreservedHeads(ctx, gateDir)
}

func ambiguousPreservedHead(refs preservedHeads, run *db.Run) (string, bool) {
	if run == nil || refs == nil || !terminalRunStatus(run.Status) || run.CustodyReturnedAt != nil {
		return "", false
	}
	if candidate, exists := refs[git.CrashHeadRef(run.ID)]; exists && candidate != run.HeadSHA {
		return candidate, true
	}
	candidate, exists := refs[git.RunHeadRef(run.ID)]
	return candidate, exists && candidate != run.HeadSHA
}

func classifyAmbiguousPreservedHead(refs preservedHeads, state *State, run *db.Run) bool {
	candidate, ambiguous := ambiguousPreservedHead(refs, run)
	if !ambiguous {
		return false
	}
	state.State = StatePipelineOwned
	state.Pipeline.Phase = "pre_push"
	state.Safety = "blocked_pipeline_owned_ambiguous"
	state.Error = fmt.Sprintf("preservation refs retain recorded head %s and a different pipeline or live worktree head %s; no head was discarded, and recovery or rerun stays blocked until an operator names the exact commit that becomes run authority (candidates: %s)", run.HeadSHA, candidate, strings.Join(candidateHeads(refs, run), ", "))
	state.NextAction = &NextAction{Code: "resolve_ambiguous_custody", Command: "no-mistakes axi sync --recover --resolve-head <exact commit to keep>"}
	return true
}

// candidateHeads lists every distinct head this run's evidence names, durable
// authority first. The order is stable so the reported choice is reproducible.
func candidateHeads(refs preservedHeads, run *db.Run) []string {
	var candidates []string
	for _, candidate := range []string{run.HeadSHA, refs[git.RunHeadRef(run.ID)], refs[git.CrashHeadRef(run.ID)]} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if !containsHead(candidates, candidate) {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func containsHead(heads []string, want string) bool {
	for _, head := range heads {
		if head == want {
			return true
		}
	}
	return false
}

// classifyCustodyReturned reports a branch whose stranded terminal run was
// explicitly recovered and never had a push binding: the operator owns the
// branch again and the only remaining step is starting a fresh run. The
// relation against the preserved pipeline head is informative only.
func (s *Service) classifyCustodyReturned(ctx context.Context, state *State) {
	state.State = StateCustodyReturned
	state.Safety = "custody_returned"
	state.Relation = RelationUnknown
	state.Error = ""
	state.NextAction = &NextAction{Code: "run_pipeline", Command: `no-mistakes axi run --intent "<what the user set out to accomplish>"`}
	preserved := state.Pipeline.CurrentHead
	if preserved == "" || !objectExists(ctx, s.workDir(), preserved) {
		return
	}
	switch {
	case state.Local.Head == preserved:
		state.Relation = RelationEqual
	case isAncestor(ctx, s.workDir(), state.Local.Head, preserved):
		state.Relation = RelationBehind
	case isAncestor(ctx, s.workDir(), preserved, state.Local.Head):
		state.Relation = RelationAhead
	default:
		state.Relation = RelationDiverged
	}
}

func pushStepRunning(database *db.DB, runID string) bool {
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return true
	}
	for _, step := range steps {
		if step.StepName == types.StepPush && (step.Status == types.StepStatusRunning || step.Status == types.StepStatusFixing) {
			return true
		}
	}
	return false
}

func objectExists(ctx context.Context, dir, sha string) bool {
	_, err := git.Run(ctx, dir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func isAncestor(ctx context.Context, dir, ancestor, descendant string) bool {
	if ancestor == "" || descendant == "" {
		return false
	}
	_, err := git.Run(ctx, dir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

func samePath(a, b string) bool {
	resolve := func(path string) string {
		abs, _ := filepath.Abs(path)
		if evaluated, err := filepath.EvalSymlinks(abs); err == nil {
			return evaluated
		}
		return abs
	}
	return resolve(a) == resolve(b)
}

func targetKind(repo *db.Repo) string {
	if repo != nil && strings.TrimSpace(repo.ForkURL) != "" {
		return "fork"
	}
	return "upstream"
}

func normalizePRState(state *string) string {
	if state == nil || strings.TrimSpace(*state) == "" {
		return "unknown"
	}
	return strings.ToLower(strings.TrimSpace(*state))
}

func ptr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func value(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func blockedPlan(state State, resultState, safety, message string) State {
	state.State = resultState
	state.Safety = safety
	state.Changed = false
	state.NextAction = nil
	state.Error = message
	return state
}
