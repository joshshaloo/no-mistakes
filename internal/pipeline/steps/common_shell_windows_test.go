//go:build windows

package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the real cmd.exe through the cooperative helper, not merely the
// wrapper's spelling or a Windows cross-compile on a Unix host.
func TestRunShellCommand_WindowsNativeExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    int
	}{
		{"success", "echo success", 0},
		{"missing command", "no_mistakes_missing_command_013", 127},
		{"ordinary failure", "cmd.exe /d /c exit 1", 1},
		{"ordinary status", "cmd.exe /d /c exit 42", 42},
		{"explicit exit", "exit 42", 42},
		{"grouped failure", "(cmd.exe /d /c exit 42)", 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			out, code, err := runShellCommand(ctx, t.TempDir(), tc.command)
			if err != nil || code != tc.want {
				t.Fatalf("command %q: exit=%d, err=%v, output=%q; want exit %d", tc.command, code, err, out, tc.want)
			}
		})
	}
}

func TestRunShellCommand_WindowsQuotedPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spaces & (parentheses) !")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	outputPath := filepath.Join(dir, "redirected output.txt")
	command := `echo pinned>"` + outputPath + `"`
	out, code, err := runShellCommand(ctx, dir, command)
	if err != nil || code != 0 {
		t.Fatalf("quoted redirect: exit=%d, err=%v, output=%q", code, err, out)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil || strings.TrimSpace(string(data)) != "pinned" {
		t.Fatalf("redirected output=%q, err=%v", data, err)
	}

	batchPath := filepath.Join(dir, "ordinary failure.cmd")
	if err := os.WriteFile(batchPath, []byte("@echo off\r\nexit /b 42\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code, err = runShellCommand(ctx, dir, `"`+batchPath+`"`)
	if err != nil || code != 42 {
		t.Fatalf("quoted batch command: exit=%d, err=%v, output=%q; want exit 42", code, err, out)
	}
}
