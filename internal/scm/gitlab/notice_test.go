package gitlab

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestListPRNoticesPaginatesExplicitly(t *testing.T) {
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab api --include projects/group%2Fproject/merge_requests/123/notes?order_by=created_at&sort=asc&per_page=100&page=1": {
			stdout: "HTTP/2.0 200 OK\nX-Next-Page: 2\n\n[{\"body\":\"first\"}]",
		},
		"glab api --include projects/group%2Fproject/merge_requests/123/notes?order_by=created_at&sort=asc&per_page=100&page=2": {
			stdout: "HTTP/2.0 200 OK\nX-Next-Page:\n\n[{\"body\":\"second\"}]",
		},
	}), nil, "", "group/project")

	got, err := host.ListPRNotices(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("notices = %#v", got)
	}
}

func TestPublishPRNoticeTargetsExactMR(t *testing.T) {
	host := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
		"glab mr note 123 --message head-bound notice": {},
	}), nil, "", "")
	if err := host.PublishPRNotice(context.Background(), &scm.PR{URL: "https://gitlab.com/test/repo/-/merge_requests/123"}, "head-bound notice"); err != nil {
		t.Fatal(err)
	}
}
