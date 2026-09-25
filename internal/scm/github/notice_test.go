package github

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPublishPRNoticeTargetsExactPR(t *testing.T) {
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr comment 123 --repo test/repo --body-file -": {},
	}), nil, "", "test/repo")
	if err := host.PublishPRNotice(context.Background(), &scm.PR{Number: "123"}, "notice"); err != nil {
		t.Fatal(err)
	}
	if err := host.PublishPRNotice(context.Background(), &scm.PR{}, "notice"); err == nil {
		t.Fatal("missing PR identity did not fail closed")
	}
}
