// Package convergence detects round-over-round non-convergence in a pipeline
// run: the condition where the fixes already applied are not settling the
// underlying cause, so the run is patching a design rather than closing
// independent defects.
//
// # This is a signal, never a gate
//
// Nothing in this package may change what a run does. It does not decide what
// parks a run, what the reviewer marks auto-fix or ask-user, what risk level a
// review assigns, what CI does, or any other existing outcome. It records a
// probability on the run record and surfaces it where a worker and a supervisor
// already look. A person reading the signal decides what to do about it.
//
// # The failure direction is inverted on purpose
//
// A guard fails safe by REFUSING. This detector fails safe by staying SILENT.
// That inversion is deliberate, and it is only correct because this is a signal.
//
// When the detector is not configured, the key cannot be resolved, the HTTP call
// errors or times out, the response body is malformed, the expected answer is
// missing, the current round has no finding, or the run has only one round, the
// detector emits nothing at all and the pipeline behaves byte-identically to a
// build without it. It never emits a guessed or default value, never blocks,
// never extends a step beyond its short
// bounded timeout, and never turns an absent answer into "converging" - absence
// of the signal means "not measured", not "measured and fine".
//
// Do not copy this fail-silent pattern into anything that actually guards. A
// guard that silently passes when its checker is unreachable is the exact bug
// the rest of this repository refuses (see the "absence of a checker is not a
// check" rule for configured commands.* tools). The pattern is safe here only
// because a missing signal removes information that nothing depends on, whereas
// a missing guard removes protection that everything depends on.
//
// # Trust boundary
//
// Every setting this package reads is host- and operator-owned (global config
// only). A repository's .no-mistakes.yaml can neither enable the detector, nor
// redirect where the key is read from, nor change where state is sent. State is
// sent to a third-party service, so a pushed branch must never be able to turn
// that on or aim it somewhere else.
package convergence
