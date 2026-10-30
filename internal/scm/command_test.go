package scm

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func TestProviderCommandExecutionCarriesRunOwnership(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, mode := range []string{"run", "output", "combined"} {
		t.Run(mode, func(t *testing.T) {
			ctx := shellenv.WithRunSupervisor(context.Background(), shellenv.NewRunSupervisor("scm-run", t.TempDir()))
			factory := SuperviseCommands(exec.CommandContext)
			cmd := factory(ctx, "sh", "-c", "printf '%s' \"$NO_MISTAKES_RUN_ID\"")
			var out []byte
			var err error
			switch mode {
			case "run":
				var buf bytes.Buffer
				cmd.Stdout = &buf
				err = cmd.Run()
				out = buf.Bytes()
			case "output":
				out, err = cmd.Output()
			case "combined":
				out, err = cmd.CombinedOutput()
			}
			if err != nil || string(out) != "scm-run" {
				t.Fatalf("ownership=%q, %v", out, err)
			}
		})
	}
}
