//go:build e2e

package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func seedLowReview(t *testing.T, sctx *pipeline.StepContext) *db.StepResult {
	t.Helper()
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"findings":[],"risk_level":"low","risk_rationale":"Test-only change without production changes"}`
	if err := sctx.DB.SetStepFindings(sr.ID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertReviewStepRound(sr.ID, 1, "initial", &raw, nil, sctx.Run.HeadSHA, 1); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteReviewStep(sr.ID, sctx.Run.ID, sctx.Run.HeadSHA, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	return sr
}

func TestCIStep_RepairInvalidatesPublishedReviewRisk(t *testing.T) {
	for _, failPR := range []bool{false, true} {
		t.Run(fmt.Sprintf("publish_failure=%v", failPR), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			upstream := t.TempDir()
			gitCmd(t, upstream, "init", "--bare")
			gitCmd(t, dir, "remote", "add", "origin", upstream)
			gitCmd(t, dir, "push", "origin", "main", "feature")
			ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				// This is an actual new file added by the CI repair agent, not a synthetic
				// head change or a direct invocation of the invalidation helper.
				if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "bin", "fm-teardown.sh"), []byte("#!/bin/sh\necho refuse to discard unlanded work\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return &agent.Result{}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			review := seedLowReview(t, sctx)
			sctx.Repo.UpstreamURL = upstream
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			sctx.Config.AutoFix.CI = 1
			sctx.Config.CITimeout = time.Minute
			sctx.Env = fakeCIGHMergeable(t, "OPEN", `[{"name":"test","bucket":"fail"}]`, "MERGEABLE")
			bodyPath := filepath.Join(t.TempDir(), "pr-body.md")
			sctx.Env = append(sctx.Env, "FAKE_CLI_RISK_BODY="+bodyPath)
			if failPR {
				sctx.Env = append(sctx.Env, "FAKE_CLI_RISK_EDIT_FAIL=1")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sctx.Ctx = ctx
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { cancel(); return ctx.Err() }}
			_, err := step.Execute(sctx)
			if failPR {
				if !errors.Is(err, errPublishStaleRisk) {
					t.Fatalf("must fail instead of reporting green after notice failure: %v", err)
				}
				if remote := gitCmd(t, upstream, "rev-parse", "feature"); remote != head {
					t.Fatal("pushed before stale rating was removed from PR")
				}
				steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(*steps[0].FindingsJSON, `"risk_level":"stale"`) {
					t.Fatal("DB lost stale risk after remote error")
				}
				return
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation after repair, got %v", err)
			}
			body, err := os.ReadFile(bodyPath)
			if err != nil {
				t.Fatalf("CI did not update PR risk: %v", err)
			}
			for _, want := range []string{"STALE", "human-authored summary", "Human intent context", "Test-only change", "Human test evidence", "Human pipeline notes"} {
				if !strings.Contains(string(body), want) {
					t.Fatalf("PR refresh did not preserve %q alongside the stale warning: %s", want, body)
				}
			}
			remote := gitCmd(t, upstream, "rev-parse", "feature")
			if remote == head {
				t.Fatal("vacuous test: CI repair was not pushed")
			}
			if got := gitCmd(t, upstream, "show", "feature:bin/fm-teardown.sh"); !strings.Contains(got, "unlanded") {
				t.Fatal(got)
			}
			steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			rounds, err := sctx.DB.GetRoundsByStep(review.ID)
			if err != nil {
				t.Fatal(err)
			}
			summary, risk := BuildPipelineSummary(steps, map[string][]*db.StepRound{review.ID: rounds})
			if !strings.Contains(strings.ToLower(risk), "stale") || strings.Contains(risk, "Test-only") {
				t.Fatalf("current risk describes old diff: %s", risk)
			}
			if strings.Contains(summary, "**Review** - passed") {
				t.Fatalf("stale review still presented as passed: %s", summary)
			}
			if len(rounds) != 1 || !strings.Contains(*rounds[0].FindingsJSON, `"risk_level":"low"`) {
				t.Fatal("original review history was lost")
			}
		})
	}
}

func TestPRStep_AgentEditsInvalidateRiskBeforePRPublication(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("production change\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: []byte(`{"title":"fix: summary","body":"## What Changed\n\nsummary"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	seedLowReview(t, sctx)
	var logFile string
	sctx.Env, logFile = fakeGH(t, "")
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := readFakeGHBodyArg(t, logFile)
	if !strings.Contains(body, "STALE") || strings.Contains(body, "Test-only") {
		t.Fatalf("PR presented pre-edit risk: %s", body)
	}
}

func TestLaterCommit_ReviewRiskClassification(t *testing.T) {
	for _, tt := range []struct {
		name, path string
		mode       os.FileMode
		stale      bool
	}{
		{"code addition", "bin/cleanup.sh", 0o644, true},
		{"test addition", "tests/cleanup.test.sh", 0o644, true},
		{"document prose", "docs/configuration.md", 0o644, false},
		{"root prose", "CONTRIBUTING.md", 0o644, false},
		{"executable docs", "docs/tool.md", 0o755, true},
		{"documentation code", "docs/example.sh", 0o644, true},
		{"agent instructions", "AGENTS.md", 0o644, true},
		{"executable markdown", "docs/component.mdx", 0o644, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			seedLowReview(t, sctx)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, tt.path)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, tt.path), []byte("changed\n"), tt.mode); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "-A")
			if tt.mode == 0o755 {
				gitCmd(t, dir, "update-index", "--chmod=+x", tt.path)
			}
			gitCmd(t, dir, "commit", "-m", "later edit")
			if err := publishPipelineHead(sctx, gitCmd(t, dir, "rev-parse", "HEAD")); err != nil {
				t.Fatal(err)
			}
			steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			f, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
			if err != nil {
				t.Fatal(err)
			}
			if got := f.RiskLevel == "stale"; got != tt.stale {
				t.Fatalf("risk = %q, want stale=%v", f.RiskLevel, tt.stale)
			}
		})
	}
}
