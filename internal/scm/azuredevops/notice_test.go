package azuredevops

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPublishPRNoticeTargetsExactPR(t *testing.T) {
	host := newTestHost(map[string]azdoTestResponse{
		"az repos pr comment create --id 123 --content head-bound notice --organization " + testOrg: {},
	})
	if err := host.PublishPRNotice(context.Background(), &scm.PR{Number: "123"}, "head-bound notice"); err != nil {
		t.Fatal(err)
	}
}
