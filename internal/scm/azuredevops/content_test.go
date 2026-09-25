package azuredevops

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestGetPRContent(t *testing.T) {
	host := newTestHost(map[string]azdoTestResponse{
		"az repos pr show --id 123 --organization " + testOrg + " --output json": {stdout: `{"pullRequestId":123,"title":"original title","description":"original\nbody"}`},
	})
	got, err := host.GetPRContent(context.Background(), &scm.PR{Number: "123"})
	if err != nil || got.Title != "original title" || got.Body != "original\nbody" {
		t.Fatalf("content=%+v, err=%v", got, err)
	}
}
