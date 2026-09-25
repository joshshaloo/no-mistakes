package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPublishRiskNotice_ReadyBoundaryRepublishesMissingHeadNotice(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	review, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"risk_level":"stale","risk_rationale":"post-review change"}`
	if err := sctx.DB.SetStepFindings(review.ID, findings); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	bodyPath := filepath.Join(t.TempDir(), "notice.md")
	sctx.Env = append(fakeCIGH(t, "OPEN", `[{"name":"test","bucket":"pass"}]`), "FAKE_CLI_RISK_BODY="+bodyPath)
	host, reason := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatal(reason)
	}
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	step := &CIStep{}
	if err := step.publishRiskNotice(sctx, host, pr, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bodyPath); err != nil {
		t.Fatal(err)
	}
	if err := step.publishRiskNotice(sctx, host, pr, true); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"STALE", head, "green CI does not make the previous rating current"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("ready notice missing %q: %s", want, body)
		}
	}
}
