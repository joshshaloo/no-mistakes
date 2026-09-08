package pipeline

import (
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// HousekeepingLintResult is the lint assessment produced by the combined
// document+lint housekeeping pass: the document step performs both duties in
// one agent invocation and hands the lint half to the lint step so it does
// not pay a second cold agent pass.
type HousekeepingLintResult struct {
	// FindingsJSON holds the lint-category findings (possibly an empty set)
	// in the same JSON shape the lint step produces itself.
	FindingsJSON string
	// Summary is the housekeeping pass's one-line lint summary.
	Summary string
}

// PostTestFixChange records pipeline-owned commits made after the Test step
// and before the network push. Such commits invalidate the earlier green test
// evidence.
//
// This marker is an in-memory optimization and log label only: the authority
// the push boundary re-verifies against is the run's durable
// test_verified_head_sha anchor, which survives a daemon restart. Losing this
// marker across a process boundary therefore costs a step-name label, never
// the invariant itself.
type PostTestFixChange struct {
	FromHead string
	ToHead   string
	Steps    []types.StepName
}

// RunShared carries in-memory run-scoped results one step hands to a later
// step in the same run. It lives on the executor for the run's lifetime and
// is never persisted: on any process boundary the consuming step simply
// falls back to doing its own work.
type RunShared struct {
	mu                sync.Mutex
	housekeepingLint  *HousekeepingLintResult
	postTestFixChange *PostTestFixChange
}

// SetHousekeepingLint records the combined pass's lint assessment for the
// lint step. It replaces any previous assessment (a document fix round
// re-runs the combined pass and re-stashes a fresh result).
func (s *RunShared) SetHousekeepingLint(result HousekeepingLintResult) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.housekeepingLint = &result
}

// ClearHousekeepingLint discards a previous combined-pass lint assessment
// before a document pass starts, so a later lint step never consumes stale
// findings.
func (s *RunShared) ClearHousekeepingLint() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.housekeepingLint = nil
}

// TakeHousekeepingLint returns and consumes the combined pass's lint
// assessment. The second call returns false so a lint fix round re-assesses
// with its own agent pass instead of trusting a stale result.
func (s *RunShared) TakeHousekeepingLint() (HousekeepingLintResult, bool) {
	if s == nil {
		return HousekeepingLintResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.housekeepingLint == nil {
		return HousekeepingLintResult{}, false
	}
	result := *s.housekeepingLint
	s.housekeepingLint = nil
	return result, true
}

// MarkPostTestFixCommit records that a post-Test pipeline step advanced HEAD.
// The first changed head is kept as the diff base so one final verification
// covers all files touched by later commits; ToHead tracks the latest head.
func (s *RunShared) MarkPostTestFixCommit(step types.StepName, fromHead, toHead string) {
	if s == nil || fromHead == "" || toHead == "" || fromHead == toHead {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.postTestFixChange == nil {
		s.postTestFixChange = &PostTestFixChange{FromHead: fromHead, ToHead: toHead, Steps: []types.StepName{step}}
		return
	}
	s.postTestFixChange.ToHead = toHead
	for _, existing := range s.postTestFixChange.Steps {
		if existing == step {
			return
		}
	}
	s.postTestFixChange.Steps = append(s.postTestFixChange.Steps, step)
}

// PendingPostTestFixChange returns the unverified post-Test commit range.
func (s *RunShared) PendingPostTestFixChange() (PostTestFixChange, bool) {
	if s == nil {
		return PostTestFixChange{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.postTestFixChange == nil {
		return PostTestFixChange{}, false
	}
	change := *s.postTestFixChange
	change.Steps = append([]types.StepName(nil), change.Steps...)
	return change, true
}

// CompletePostTestFixVerification clears the pending marker once the current
// final head has been re-verified and recorded.
func (s *RunShared) CompletePostTestFixVerification() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.postTestFixChange = nil
}
