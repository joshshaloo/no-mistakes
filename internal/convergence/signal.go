package convergence

// Finding is one review finding as the detector sees it. It carries only the
// fields that help judge cause, never a diff, a patch, or a file body.
type Finding struct {
	ID          string `json:"id,omitempty"`
	Severity    string `json:"severity,omitempty"`
	File        string `json:"file,omitempty"`
	Action      string `json:"action,omitempty"`
	Description string `json:"description,omitempty"`
}

// Round is one execution round of a step: what it found, and - for a fix round
// - the one-line summary of what it changed.
type Round struct {
	Number         int       `json:"round"`
	Trigger        string    `json:"trigger,omitempty"`
	FixSummary     string    `json:"fix_summary,omitempty"`
	Findings       []Finding `json:"findings,omitempty"`
	SelectedForFix []string  `json:"selected_for_fix,omitempty"`
}

// Observation is the run's own history, which is the only thing the question is
// asked over: the accepted intent, the current round's findings, and every
// earlier round of the same step.
type Observation struct {
	Intent  string
	Step    string
	Current Round
	Earlier []Round
}

// Signal is a recorded non-convergence judgment.
//
// Probability is kept rather than a boolean so a threshold can be tuned later
// without re-running anything, and Model is the exact responding model version
// so a decision made from this signal can be traced back to what produced it.
type Signal struct {
	// Probability is the model's probability that the fixes applied so far have
	// failed to settle the underlying cause behind the current finding.
	Probability float64
	// Model is the versioned model that answered, as the service reported it.
	Model string
	// Step and Round locate the round the judgment was made after.
	Step  string
	Round int
	// CausalThemes is the secondary Score: roughly how many distinct causal
	// themes the run has accumulated. It is nil when that answer was absent;
	// a missing Score does not suppress the Noul.
	CausalThemes          *float64
	CausalThemeConfidence *float64
	// ObservedAt is unix seconds.
	ObservedAt int64
}
