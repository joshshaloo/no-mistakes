package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLegacyPRReceivesV1AttestationWithoutRedrafting(t *testing.T) {
	body := "## Author plan\n\nKeep this narrative and closing references.\nCloses test/repo#42\n\n" + noMistakesPRSignature
	steps := []*db.StepResult{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted},
		{StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}
	updated, changed, err := rebindOwnedPRAttestation(body, testPipelineHeadSHA, steps, pipelineAttestationPolicy{})
	if err != nil || !changed {
		t.Fatalf("legacy upgrade: changed=%v err=%v", changed, err)
	}
	if !strings.HasPrefix(updated, body+"\n\n") {
		t.Fatalf("author content changed: %s", updated)
	}
	if got, output := runVerifyPy(t, updated, testPipelineHeadSHA); got != "success" {
		t.Fatalf("v1 verification = %s: %s", got, output)
	}
	again, changed, err := rebindOwnedPRAttestation(updated, testPipelineHeadSHA, steps, pipelineAttestationPolicy{})
	if err != nil || !changed || again != updated {
		t.Fatalf("repeated upgrade not idempotent: %v %v\n%s", changed, err, again)
	}
}

func TestLegacyPRCarryOnlyRestampCannotMintAttestation(t *testing.T) {
	body := "Author prose\n\n" + noMistakesPRSignature
	updated, changed, err := rebindOwnedPRAttestation(body, testPipelineHeadSHA, nil, pipelineAttestationPolicy{})
	if err != nil || changed || updated != body {
		t.Fatalf("carry-only path minted authority: %v %v %s", changed, err, updated)
	}
}

func TestLegacyPRAttestationDoesNotInventSuccessfulSteps(t *testing.T) {
	body := "Author prose\n\n" + noMistakesPRSignature
	updated, changed, err := rebindOwnedPRAttestation(body, testPipelineHeadSHA, []*db.StepResult{{StepName: types.StepReview, Status: types.StepStatusCompleted}}, pipelineAttestationPolicy{})
	if err != nil || !changed {
		t.Fatalf("upgrade failed: %v %v", changed, err)
	}
	if got, output := runVerifyPy(t, updated, testPipelineHeadSHA); got != "failure" {
		t.Fatalf("incomplete run passed attestation: %s %s", got, output)
	}
}
