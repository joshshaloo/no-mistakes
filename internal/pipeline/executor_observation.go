package pipeline

import "github.com/kunchenguid/no-mistakes/internal/convergence"

// SetBeforeComplete installs the run owner's idempotent resource cleanup.
// Success must wait for it; failure and panic paths also invoke it from defer.
func (e *Executor) SetBeforeComplete(cleanup func() error) {
	e.completionFinalizer.Cleanup = cleanup
}

// SetConvergenceDetector substitutes an advisory detector (tests use local HTTP).
func (e *Executor) SetConvergenceDetector(d *convergence.Detector) {
	e.convergence = d
}
