package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func nonconvergenceRunView() runView {
	themes := 1.42
	return runView{
		ID:      "run-1",
		Branch:  "feature/x",
		Status:  string(types.RunRunning),
		HeadSHA: "abcdef1234567890",
		Nonconvergence: &nonconvergenceView{
			Probability:  0.88,
			Model:        "typesafe/jev-1.13-20260917",
			Round:        3,
			CausalThemes: &themes,
		},
		Steps: []stepView{{Name: "review", Status: "awaiting_approval"}},
	}
}

func TestRunObjectRendersNonconvergenceAsAnAdvisorySignal(t *testing.T) {
	out := axiDoc(runObjectField(nonconvergenceRunView()))

	for _, want := range []string{
		"nonconvergence: ",
		"p=0.88",
		"after round 3",
		"causal_themes=1.42",
		"model typesafe/jev-1.13-20260917",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run object missing %q in:\n%s", want, out)
		}
	}
	// The reader must be told the pipeline did not act on it, at the point of
	// use, so nobody mistakes it for a gate the run already applied.
	if !strings.Contains(out, "gates nothing") {
		t.Errorf("the signal must say outright that it gates nothing, in:\n%s", out)
	}
}

func TestRunObjectOmitsNonconvergenceWhenNotMeasured(t *testing.T) {
	rv := nonconvergenceRunView()
	rv.Nonconvergence = nil
	if out := axiDoc(runObjectField(rv)); strings.Contains(out, "nonconvergence") {
		t.Errorf("an unmeasured run must render no signal at all in:\n%s", out)
	}
}

func TestRunViewCarriesNonconvergenceFromIPCAndDB(t *testing.T) {
	themes := 0.5
	fromIPC := runViewFromIPC(&ipc.RunInfo{
		ID:     "r",
		Status: types.RunRunning,
		Nonconvergence: &ipc.NonconvergenceInfo{
			Probability: 0.7, Model: "m", Round: 2, CausalThemes: &themes,
		},
	})
	if fromIPC.Nonconvergence == nil || fromIPC.Nonconvergence.Probability != 0.7 || fromIPC.Nonconvergence.Model != "m" {
		t.Fatalf("IPC view lost the signal: %+v", fromIPC.Nonconvergence)
	}

	prob := 0.7
	model := "m"
	step := "review"
	round := 2
	observed := int64(5)
	fromDB := runViewFromDB(&db.Run{
		ID:                        "r",
		Status:                    types.RunRunning,
		NonconvergenceProbability: &prob,
		NonconvergenceModel:       &model,
		NonconvergenceStep:        &step,
		NonconvergenceRound:       &round,
		NonconvergenceThemes:      &themes,
		NonconvergenceObservedAt:  &observed,
	}, nil)
	if fromDB.Nonconvergence == nil || fromDB.Nonconvergence.Probability != 0.7 || fromDB.Nonconvergence.Round != 2 {
		t.Fatalf("DB view lost the signal: %+v", fromDB.Nonconvergence)
	}

	// Absence stays absence on both paths.
	if runViewFromIPC(&ipc.RunInfo{ID: "r"}).Nonconvergence != nil {
		t.Error("an IPC run with no signal must produce no view")
	}
	if runViewFromDB(&db.Run{ID: "r"}, nil).Nonconvergence != nil {
		t.Error("a DB run with no signal must produce no view")
	}
}
