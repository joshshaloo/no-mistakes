package gitlab

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPublishPRNoticeTargetsExactMR(t *testing.T) {
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr note 123 --message head-bound notice": {},
	}), nil, "", "")
	if err := host.PublishPRNotice(context.Background(), &scm.PR{URL: "https://gitlab.com/test/repo/-/merge_requests/123"}, "head-bound notice"); err != nil {
		t.Fatal(err)
	}
}
