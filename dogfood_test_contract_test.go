package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// Local Test is targeted validation of the requested intent. This repository
// must not dogfood a complete-suite commands.test override (historically
// go test -race ./...), which burned multi-hour local walks while remote CI
// already owns the race-enabled full suite.
func TestDogfoodConfig_NoBroadLocalTestCommand(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadRepo(root)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	if got := strings.TrimSpace(cfg.Commands.Test); got != "" {
		t.Fatalf("dogfood commands.test = %q, want empty so local Test stays agent-targeted; put broad regression in remote CI", got)
	}

	raw, err := os.ReadFile(filepath.Join(root, ".no-mistakes.yaml"))
	if err != nil {
		t.Fatalf("read .no-mistakes.yaml: %v", err)
	}
	content := string(raw)
	for _, forbid := range []string{
		`test: "go test -race ./..."`,
		`test: 'go test -race ./...'`,
		`test: go test -race ./...`,
	} {
		if strings.Contains(content, forbid) {
			t.Fatalf(".no-mistakes.yaml still configures broad local Test %q", forbid)
		}
	}
}

// Repository CI failures must park rather than let an unscoped repair undo
// reviewed release metadata. Explicit zero must beat an operator's override.
func TestDogfoodConfig_CIAutoRepairDisabled(t *testing.T) {
	cfg, err := config.LoadRepo(".")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoFix.CI == nil || *cfg.AutoFix.CI != 0 {
		t.Fatal("dogfood auto_fix.ci must be explicitly zero")
	}
	globalLimit := 3
	effective := config.EffectiveRepoConfig(cfg, &config.RepoConfig{}, false)
	merged := config.Merge(&config.GlobalConfig{AutoFix: config.AutoFixRaw{CI: &globalLimit}}, effective)
	if merged.AutoFix.CI != 0 {
		t.Fatalf("effective CI repair limit = %d, want 0", merged.AutoFix.CI)
	}
}

// commands.test contract ownership lives in repo-config.md: targeted local
// validation, not CI-parity complete-suite configuration, and no brittle
// shell-heuristics claiming to detect "broad" commands.
func TestRepoConfigDocs_CommandsTestIsTargetedContract(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("docs", "src", "content", "docs", "reference", "repo-config.md"))
	if err != nil {
		t.Fatalf("read repo-config.md: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"### commands.test",
		"targeted validation",
		"not a CI-parity repository-wide regression command",
		"Broad regression belongs in remote CI",
		"does not guess whether an arbitrary shell string is \"too broad\"",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("repo-config.md commands.test contract missing %q", want)
		}
	}
}

// Remote CI must keep the complete race-enabled Go suite as the broad
// regression owner after local Test stops duplicating it.
func TestCIWorkflow_RetainsFullRaceSuiteAsBroadRegressionOwner(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "run: go test -race ./...") {
		t.Fatal("CI workflow must still run go test -race ./... as the broad regression owner")
	}
	if !strings.Contains(content, "if: runner.os != 'Windows'") {
		t.Fatal("CI workflow must keep the Unix race-test branch")
	}
}
