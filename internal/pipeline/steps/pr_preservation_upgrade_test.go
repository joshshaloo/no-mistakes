package steps

import (
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestPRStep_ExistingUnconfiguredPRUsesNoticeNotRedrafting(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	env, logPath := fakeGH(t, "https://github.com/test/repo/pull/42")
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Env = env
	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil || outcome == nil {
		t.Fatalf("reuse: %+v %v", outcome, err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("existing human PR redrafted in %d agent calls", len(ag.calls))
	}
	if strings.Contains(string(data), "pr edit") || !strings.Contains(string(data), "pr comment") {
		t.Fatalf("expected only conversation notice, got:\n%s", data)
	}
}
