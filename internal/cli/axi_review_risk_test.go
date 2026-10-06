package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func TestAxiStaleReviewRiskAtChecksPassedAndCompletion(t *testing.T) {
	raw := `{"findings":[],"risk_level":"stale","risk_rationale":"Post-review changes require review"}`
	for _, ready := range []bool{true, false} {
		run := &ipc.RunInfo{ID: "r", Status: types.RunCompleted, Steps: []ipc.StepResultInfo{{StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &raw}}}
		if ready {
			run.Status = types.RunRunning
		}
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := renderDriveResult(cmd, run, ready); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"risk: stale", "do not use the previous rating as merge authority"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("missing %q: %s", want, out.String())
			}
		}
		if strings.Contains(out.String(), "the PR is ready") {
			t.Errorf("stale assessment still called ready: %s", out.String())
		}
	}
}
