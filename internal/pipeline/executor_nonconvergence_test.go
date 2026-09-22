package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/convergence"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The whole contract of this feature is that it is a signal, never a gate. Every
// test below therefore asserts one of two things: that a measurement was
// recorded, or that the run came out byte-identical to a run with no detector
// at all.

const nonconvergenceAnswer = `{"model":"typesafe/jev-1.13-20260917","answers":{"non_convergence":{"type":"noul","noul":0.88},"causal_themes":{"type":"score","score":1.31,"confidence":0.77}}}`

// detectorServing wires an executor-ready detector onto a local test server and
// reports how many times the service was actually reached.
func detectorServing(t *testing.T, handler http.HandlerFunc) (*convergence.Detector, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("OPENROUTER_API_KEY", "test-key")
	d := convergence.New(convergence.Settings{Enabled: true, BaseURL: srv.URL, Timeout: time.Second})
	if d == nil {
		t.Fatal("expected a detector")
	}
	d.SetHTTPClient(srv.Client())
	return d, &calls
}

func answeringDetector(t *testing.T) (*convergence.Detector, *int32) {
	t.Helper()
	return detectorServing(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(nonconvergenceAnswer))
	})
}

// twoRoundReviewStep parks once with an auto-fixable finding (driving a second
// round through auto-fix) and then passes.
func twoRoundReviewStep(calls *int) Step {
	return &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			*calls++
			if *calls == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings:      `{"findings":[{"id":"f1","severity":"error","description":"leaks grandchildren","action":"auto-fix"}],"summary":"1 issue"}`,
				}, nil
			}
			return &StepOutcome{
				AutoFixable: true,
				FixSummary:  "add process group",
				Findings:    `{"findings":[{"id":"f2","severity":"error","description":"still leaks at a sibling site","action":"no-op"}],"summary":"1 issue"}`,
			}, nil
		},
	}
}

// runSnapshot is everything about a finished run that the signal must never
// perturb.
type runSnapshot struct {
	Status      types.RunStatus
	Error       *string
	StepCount   int
	StepStatus  []types.StepStatus
	StepExit    []*int
	StepFinding []*string
	RoundCount  int
	ExecErr     string
	StepCalls   int
}

func snapshotRun(t *testing.T, database *db.DB, runID string, execErr error, stepCalls int) runSnapshot {
	t.Helper()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	snap := runSnapshot{Status: run.Status, Error: run.Error, StepCount: len(steps), StepCalls: stepCalls}
	for _, s := range steps {
		snap.StepStatus = append(snap.StepStatus, s.Status)
		snap.StepExit = append(snap.StepExit, s.ExitCode)
		snap.StepFinding = append(snap.StepFinding, s.FindingsJSON)
		rounds, err := database.GetRoundsByStep(s.ID)
		if err != nil {
			t.Fatal(err)
		}
		snap.RoundCount += len(rounds)
	}
	if execErr != nil {
		snap.ExecErr = execErr.Error()
	}
	return snap
}

func equalSnapshots(t *testing.T, want, got runSnapshot, context string) {
	t.Helper()
	if want.Status != got.Status {
		t.Errorf("%s: run status %q, want %q", context, got.Status, want.Status)
	}
	if want.ExecErr != got.ExecErr {
		t.Errorf("%s: execute error %q, want %q", context, got.ExecErr, want.ExecErr)
	}
	if want.StepCount != got.StepCount || want.RoundCount != got.RoundCount || want.StepCalls != got.StepCalls {
		t.Errorf("%s: steps/rounds/calls = %d/%d/%d, want %d/%d/%d",
			context, got.StepCount, got.RoundCount, got.StepCalls, want.StepCount, want.RoundCount, want.StepCalls)
	}
	for i := range want.StepStatus {
		if i >= len(got.StepStatus) {
			break
		}
		if want.StepStatus[i] != got.StepStatus[i] {
			t.Errorf("%s: step %d status %q, want %q", context, i, got.StepStatus[i], want.StepStatus[i])
		}
		if !equalIntPtr(want.StepExit[i], got.StepExit[i]) {
			t.Errorf("%s: step %d exit code differs", context, i)
		}
		if !equalStrPtr(want.StepFinding[i], got.StepFinding[i]) {
			t.Errorf("%s: step %d findings differ", context, i)
		}
	}
}

func equalIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// runTwoRoundReview executes a two-round review under the given detector and
// returns the snapshot plus the finished run.
func runTwoRoundReview(t *testing.T, detector *convergence.Detector) (runSnapshot, *db.Run, *db.DB) {
	t.Helper()
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	calls := 0
	exec := NewExecutor(database, p, cfg, nil, []Step{twoRoundReviewStep(&calls), newPassStep(types.StepTest)}, nil)
	exec.SetConvergenceDetector(detector)

	execErr := exec.Execute(context.Background(), run, repo, workDir)
	snap := snapshotRun(t, database, run.ID, execErr, calls)
	finished, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return snap, finished, database
}

// --- the signal is recorded when the call succeeds ---

func TestExecutor_RecordsNonconvergenceWithProbabilityAndModelVersion(t *testing.T) {
	detector, calls := answeringDetector(t)
	_, finished, _ := runTwoRoundReview(t, detector)

	if atomic.LoadInt32(calls) == 0 {
		t.Fatal("expected the detector to be consulted after the second round")
	}
	nc := finished.Nonconvergence()
	if nc == nil {
		t.Fatal("expected a recorded non-convergence signal")
	}
	if nc.Probability != 0.88 {
		t.Errorf("probability = %v, want 0.88 stored as a probability, not a boolean", nc.Probability)
	}
	if nc.Model != "typesafe/jev-1.13-20260917" {
		t.Errorf("model = %q, want the exact responding model version", nc.Model)
	}
	if nc.Step != string(types.StepReview) {
		t.Errorf("step = %q, want review", nc.Step)
	}
	if nc.Round < 2 {
		t.Errorf("round = %d, want a round beyond the first", nc.Round)
	}
	if nc.Themes == nil || *nc.Themes != 1.31 {
		t.Errorf("causal themes = %v, want the score recorded alongside the noul", nc.Themes)
	}
}

// --- a single-round run emits nothing ---

func TestExecutor_SingleRoundRunEmitsNoNonconvergenceSignal(t *testing.T) {
	detector, calls := answeringDetector(t)

	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{newPassStep(types.StepReview)}, nil)
	exec.SetConvergenceDetector(detector)

	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	finished, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Nonconvergence() != nil {
		t.Fatal("a single-round run must record nothing")
	}
	if atomic.LoadInt32(calls) != 0 {
		t.Fatalf("a single-round run must not reach the service, got %d calls", atomic.LoadInt32(calls))
	}
}

// --- every failure mode is silent and leaves the run identical ---

func TestExecutor_RejectedNonconvergenceResponseLogsConcreteReason(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	detector, _ := detectorServing(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev","answers":{"non_convergence":{"type":"score","noul":0.7}}}`))
	})
	runTwoRoundReview(t, detector)

	got := logs.String()
	if !strings.Contains(got, "non-convergence signal unavailable") || !strings.Contains(got, "type is") {
		t.Fatalf("rejected response reason was not observable in one log line: %q", got)
	}
}

func TestExecutor_NonconvergenceFailuresLeaveTheRunByteIdentical(t *testing.T) {
	// Baseline: exactly today's behavior, with no detector at all.
	baseline, baselineRun, _ := runTwoRoundReview(t, nil)
	if baselineRun.Nonconvergence() != nil {
		t.Fatal("baseline must carry no signal")
	}

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"http error", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}},
		{"rate limited", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
		}},
		{"malformed body", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{not json"))
		}},
		{"missing answer", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"model":"jev","answers":{"causal_themes":{"type":"score","score":1}}}`))
		}},
		{"empty body", func(w http.ResponseWriter, r *http.Request) {}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detector, _ := detectorServing(t, tc.handler)
			got, finished, _ := runTwoRoundReview(t, detector)

			if finished.Nonconvergence() != nil {
				t.Fatalf("%s must record nothing; an absent answer is never turned into a value", tc.name)
			}
			equalSnapshots(t, baseline, got, tc.name)
		})
	}
}

func TestExecutor_UnconfiguredDetectorLeavesTheRunByteIdentical(t *testing.T) {
	baseline, _, _ := runTwoRoundReview(t, nil)

	// No key configured: New returns nil, which is exactly the nil detector.
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	d := convergence.New(convergence.Settings{Enabled: true, BaseURL: "https://example.invalid"})
	if d != nil {
		t.Fatal("an enabled detector with no key must be nil")
	}
	got, finished, _ := runTwoRoundReview(t, d)

	if finished.Nonconvergence() != nil {
		t.Fatal("an unconfigured host must record nothing")
	}
	equalSnapshots(t, baseline, got, "unconfigured")
}

func TestExecutor_NonconvergenceIsOffByDefaultInConfig(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "present-but-unused")
	database, p, run, repo := setupTest(t)

	// A default merged config, as a host that configured nothing produces.
	cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
	if cfg.Nonconvergence.Enabled {
		t.Fatal("the non-convergence signal must default to off")
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{newPassStep(types.StepReview)}, nil)
	if exec.convergence != nil {
		t.Fatal("a default config must build no detector, even with a key in the environment")
	}
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	finished, _ := database.GetRun(run.ID)
	if finished.Nonconvergence() != nil {
		t.Fatal("off by default must mean nothing recorded")
	}
}

// --- the call is bounded ---

func TestExecutor_SlowNonconvergenceServiceDoesNotExtendTheStep(t *testing.T) {
	baseline, _, _ := runTwoRoundReview(t, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	d := convergence.New(convergence.Settings{Enabled: true, BaseURL: srv.URL, Timeout: 100 * time.Millisecond})
	if d == nil {
		t.Fatal("expected a detector")
	}
	d.SetHTTPClient(srv.Client())

	start := time.Now()
	got, finished, _ := runTwoRoundReview(t, d)
	elapsed := time.Since(start)

	if finished.Nonconvergence() != nil {
		t.Fatal("a service that never answers must record nothing")
	}
	// Two review rounds means at most one detector call, bounded at 100ms.
	if elapsed > 5*time.Second {
		t.Fatalf("a hung service extended the run to %v; the call must stay bounded", elapsed)
	}
	equalSnapshots(t, baseline, got, "slow service")
}

// --- the signal never moves a gate ---

func TestExecutor_HighNonconvergenceStillParksExactlyAsBefore(t *testing.T) {
	// The detector is emphatic that the run is not converging. The ask-user
	// gate must behave exactly as it does without it: park, wait, and resolve
	// only on the responder's action.
	detector, _ := detectorServing(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev","answers":{"non_convergence":{"type":"noul","noul":0.99}}}`))
	})

	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	calls := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls++
			switch calls {
			case 1:
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings:      `{"findings":[{"id":"f1","severity":"error","description":"bug","action":"auto-fix"}]}`,
				}, nil
			default:
				// Round two parks for a human decision.
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"id":"f2","severity":"error","description":"is this intended?","action":"ask-user"}]}`,
				}, nil
			}
		},
	}
	exec := NewExecutor(database, p, cfg, nil, []Step{step, newPassStep(types.StepTest)}, nil)
	exec.SetConvergenceDetector(detector)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)

	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Status != types.RunRunning {
		t.Errorf("run status while parked = %q, want %q: the signal must not change run state", parked.Status, types.RunRunning)
	}
	if parked.AwaitingAgentSince == nil {
		t.Error("the run must still be marked parked awaiting the agent")
	}
	// The signal is present and says "not converging" - and the gate is still
	// exactly where it was, waiting for a person.
	if nc := parked.Nonconvergence(); nc == nil || nc.Probability != 0.99 {
		t.Fatalf("expected the high signal to be recorded, got %+v", nc)
	}

	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("respond: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("executor timed out")
	}

	finished, _ := database.GetRun(run.ID)
	if finished.Status != types.RunCompleted {
		t.Errorf("run status = %q, want %q: only the responder's action resolves the gate", finished.Status, types.RunCompleted)
	}
	steps, _ := database.GetStepsByRun(run.ID)
	for _, s := range steps {
		if s.Status != types.StepStatusCompleted {
			t.Errorf("step %s = %q, want completed", s.StepName, s.Status)
		}
	}
}

// --- the detector only runs for the step that owns the question ---

func TestExecutor_NonconvergenceIsNotAskedForNonReviewSteps(t *testing.T) {
	detector, calls := answeringDetector(t)

	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{AutoFix: config.AutoFix{Lint: 3}}

	lintCalls := 0
	step := &adaptiveCallStep{
		name: types.StepLint,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			lintCalls++
			if lintCalls == 1 {
				return &StepOutcome{
					AutoFixable: true,
					Findings:    `{"findings":[{"id":"l1","severity":"error","description":"unused import","action":"auto-fix"}]}`,
				}, nil
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	exec.SetConvergenceDetector(detector)

	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if atomic.LoadInt32(calls) != 0 {
		t.Fatalf("only review rounds carry the question, got %d calls", atomic.LoadInt32(calls))
	}
	finished, _ := database.GetRun(run.ID)
	if finished.Nonconvergence() != nil {
		t.Fatal("a non-review step must record nothing")
	}
}
