package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/skill"
)

func TestCustodyGuidanceRefusesUnarchivableMissingWork(t *testing.T) {
	human, axi := newSyncCmd(), newAxiSyncCmd()
	for name, text := range map[string]string{
		"skill":    skill.Markdown(),
		"live":     branchSyncAgentGuidance,
		"sync":     human.Long + human.Flags().Lookup("keep-local").Usage,
		"axi sync": axi.Long + axi.Flags().Lookup("keep-local").Usage,
	} {
		if !strings.Contains(text, "unarchivable") {
			t.Errorf("%s does not explain the archive requirement", name)
		}
		for _, wrong := range []string{"discard genuinely missing", "explicitly discarding those unpublished", "discarding the missing commits", "missing preserved commits are discarded"} {
			if strings.Contains(text, wrong) {
				t.Errorf("%s advertises unsafe missing-head discard: %s", name, wrong)
			}
		}
	}
}
