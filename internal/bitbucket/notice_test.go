package bitbucket

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type noticeAPI struct {
	API
	id   int
	body string
}

func (a *noticeAPI) AddPRComment(_ context.Context, _ RepoRef, id int, body string) error {
	a.id, a.body = id, body
	return nil
}

func TestPublishPRNoticeAppendsComment(t *testing.T) {
	api := &noticeAPI{}
	host := NewHost(api, RepoRef{Workspace: "test", RepoSlug: "repo"})
	if err := host.PublishPRNotice(context.Background(), &scm.PR{Number: "123"}, "head-bound notice"); err != nil {
		t.Fatal(err)
	}
	if api.id != 123 || api.body != "head-bound notice" {
		t.Fatalf("comment = (%d, %q)", api.id, api.body)
	}
}
