//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// TestStartServerWithPort_StampsRunMarkerIntoServerEnvironment closes the
// managed-server gap in the ownership proof. The opencode and rovodev adapters
// run every agent invocation through this server rather than through
// StartShellCommand, so without the marker here nothing in their process tree
// could be recognised as run-owned and an escapee would be undiscoverable.
func TestStartServerWithPort_StampsRunMarkerIntoServerEnvironment(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "child.env")
	bin := writeEnvDumpingServerBin(t, dir, envPath)

	supervisor := shellenv.NewRunSupervisor("run-managed-server", dir)
	ctx := shellenv.WithRunSupervisor(context.Background(), supervisor)

	// The fake server dumps its child's environment and exits, so startup fails
	// on the early-exit path instead of waiting out the health deadline.
	srv, err := startServerWithPort(ctx, "test", bin, nil, dir, "/healthcheck", 1)
	if err == nil {
		srv.shutdown()
		t.Fatal("expected startup to fail when the fake server exits immediately")
	}

	data, readErr := os.ReadFile(envPath)
	if readErr != nil {
		t.Fatalf("fake server never recorded its child environment: %v", readErr)
	}
	child := string(data)
	if !strings.Contains(child, shellenv.RunIDEnvVar+"=run-managed-server") {
		t.Fatalf("managed-server child is missing %s:\n%s", shellenv.RunIDEnvVar, child)
	}
}

func TestStartServerWithPort_LeavesEnvironmentUnmarkedOutsideARun(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "child.env")
	bin := writeEnvDumpingServerBin(t, dir, envPath)

	srv, err := startServerWithPort(context.Background(), "test", bin, nil, dir, "/healthcheck", 1)
	if err == nil {
		srv.shutdown()
		t.Fatal("expected startup to fail when the fake server exits immediately")
	}

	data, readErr := os.ReadFile(envPath)
	if readErr != nil {
		t.Fatalf("fake server never recorded its child environment: %v", readErr)
	}
	if strings.Contains(string(data), shellenv.RunIDEnvVar+"=") {
		t.Fatalf("server started outside a run was stamped with a run marker:\n%s", data)
	}
}

// writeEnvDumpingServerBin returns a fake server that records the environment of
// a child process it spawns, proving the marker is inherited rather than merely
// present on the server process itself.
func writeEnvDumpingServerBin(t *testing.T, dir, envPath string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-server.sh")
	script := "#!/bin/sh\nsh -c env > \"" + envPath + "\"\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
