package azuredevops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func TestAzureCommandsCarryRunOwnership(t *testing.T) {
	ctx := shellenv.WithRunSupervisor(context.Background(), shellenv.NewRunSupervisor("azure-run", t.TempDir()))
	h := New(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAzureRunMarkerHelper$")
		cmd.Env = append(os.Environ(), "NM_AZURE_MARKER_HELPER=1")
		return cmd
	}, func() bool { return true }, testOrg, testProject, testRepo)
	if err := h.Available(ctx); err != nil {
		t.Fatalf("auth/extension probes lost ownership: %v", err)
	}
	if _, err := h.FindPR(ctx, "feature", "main"); err != nil {
		t.Fatalf("JSON command lost ownership: %v", err)
	}
}

func TestAzureRunMarkerHelper(t *testing.T) {
	if os.Getenv("NM_AZURE_MARKER_HELPER") != "1" {
		return
	}
	if os.Getenv(shellenv.RunIDEnvVar) != "azure-run" {
		fmt.Fprintln(os.Stderr, "missing run ownership marker")
		os.Exit(1)
	}
	fmt.Println("[]")
	os.Exit(0)
}
