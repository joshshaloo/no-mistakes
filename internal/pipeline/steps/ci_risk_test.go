package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type apparentAttemptIdentityHost struct {
	scm.Host
	provider scm.Provider
	identity string
}

func (h apparentAttemptIdentityHost) Provider() scm.Provider { return h.provider }
func (h apparentAttemptIdentityHost) GetCIAttemptIdentity(context.Context, *scm.PR, string, []scm.Check) (string, error) {
	return h.identity, nil
}

func TestCurrentCIAttemptIdentity_AllowsOnlyVerifiedProviderContracts(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	githubHost, reason := buildHost(sctx, scm.ProviderGitHub)
	if githubHost == nil {
		t.Fatal(reason)
	}
	checks := []scm.Check{{Name: "build", AttemptID: "convincing-id", DetailsURL: "https://example.test/run/1"}}
	for _, provider := range []scm.Provider{scm.ProviderGitLab, scm.ProviderBitbucket, scm.ProviderAzureDevOps, scm.ProviderUnknown, scm.Provider("future-provider")} {
		host := apparentAttemptIdentityHost{Host: githubHost, provider: provider, identity: "apparently-authoritative-attempt"}
		if _, err := currentCIAttemptIdentity(context.Background(), host, &scm.PR{Number: "42"}, head, checks); err == nil || !strings.Contains(err.Error(), "verified CI attempt identity contract") {
			t.Fatalf("provider %q was not refused by default: %v", provider, err)
		}
	}
}

func TestCIAttemptIdentity_RequiresProviderIdentityAndChangesAcrossReruns(t *testing.T) {
	first, err := ciAttemptIdentity([]scm.Check{{Name: "build", AttemptID: "run-1"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ciAttemptIdentity([]scm.Check{{Name: "build", AttemptID: "run-2"}})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("different provider attempts collapsed to %q", first)
	}
	if _, err := ciAttemptIdentity([]scm.Check{{Name: "build"}}); err == nil || !strings.Contains(err.Error(), "omitted attempt identity") {
		t.Fatalf("ambiguous attempt identity did not fail closed: %v", err)
	}
}

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
	if err := step.reconcileRiskNotice(sctx, host, pr, "CI ready: checks passed", "attempt-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bodyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bodyPath + ".jsonl"); err != nil {
		t.Fatal(err)
	}
	if err := step.reconcileRiskNotice(sctx, host, pr, "CI ready: checks passed", "attempt-1"); err != nil {
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
	t.Logf("DELETED/RECONCILED notices=1 head=%s", head)
}

func TestReconcileRiskNotice_CurrentStateLifecycle(t *testing.T) {
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
	count := func() int {
		data, err := os.ReadFile(bodyPath + ".jsonl")
		if err != nil {
			t.Fatal(err)
		}
		return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
	}
	if err := step.reconcileRiskNotice(sctx, host, pr, "CI ready: checks passed", "attempt-1"); err != nil {
		t.Fatal(err)
	}
	if err := step.reconcileRiskNotice(sctx, host, pr, "CI checks pending", "attempt-2"); err != nil {
		t.Fatal(err)
	}
	if err := step.reconcileRiskNotice(sctx, host, pr, "CI ready: checks passed", "attempt-2"); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 3 {
		t.Fatalf("pending-to-green notices = %d, want 3", got)
	}
	if err := step.reconcileRiskNotice(sctx, host, pr, "CI ready: checks passed", "attempt-2"); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 3 {
		t.Fatalf("healthy unchanged poll duplicated notices: %d", got)
	}
	t.Logf("PENDING/GREEN/RECONCILED notices=%d head=%s; unchanged-control=%d", count(), head, count())

	f, err := os.Create(bodyPath + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	incomplete := fmt.Sprintf("<!-- no-mistakes-validation run=%s head=%s -->\nincomplete", sctx.Run.ID, head)
	if err := json.NewEncoder(f).Encode(map[string]string{"body": incomplete}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := step.reconcileRiskNotice(sctx, host, pr, "CI ready: checks passed", "attempt-2"); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 2 {
		t.Fatalf("incomplete current notice was not reconciled: count=%d", got)
	}
}

func TestReconcileRiskNotice_LookupFailureFailsClosed(t *testing.T) {
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
	sctx.Env = append(fakeCIGH(t, "OPEN", `[]`), "FAKE_CLI_RISK_BODY="+bodyPath, "FAKE_CLI_RISK_LOOKUP_FAIL=1")
	host, reason := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatal(reason)
	}
	err = (&CIStep{}).reconcileRiskNotice(sctx, host, &scm.PR{Number: "42"}, "CI ready: checks passed", "attempt-1")
	if !errors.Is(err, errPublishStaleRisk) || !strings.Contains(err.Error(), "read PR validation notices") {
		t.Fatalf("lookup error did not fail closed: %v", err)
	}
	if _, statErr := os.Stat(bodyPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("notice published after lookup failure: %v", statErr)
	}
	t.Logf("LOOKUP-FAIL-CLOSED notices=0 head=%s CIReady=false", head)
}
