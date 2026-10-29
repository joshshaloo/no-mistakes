package scm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func TestSSHHostLookupCarriesRunOwnership(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n[ \"$NO_MISTAKES_RUN_ID\" = scm-ssh ] || exit 1\nprintf 'hostname github.com\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ctx := shellenv.WithRunSupervisor(context.Background(), shellenv.NewRunSupervisor("scm-ssh", dir))
	host, err := lookupSSHHostname(ctx, "alias")
	if err != nil || host != "github.com" {
		t.Fatalf("supervised SSH lookup = %q, %v", host, err)
	}
}
