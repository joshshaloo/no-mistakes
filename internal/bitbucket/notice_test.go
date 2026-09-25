package bitbucket

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type noticeAPI struct {
	API
	id       int
	body     string
	statuses []CommitStatus
}

func (a *noticeAPI) ListPRStatuses(context.Context, RepoRef, int) ([]CommitStatus, error) {
	return a.statuses, nil
}

func (a *noticeAPI) AddPRComment(_ context.Context, _ RepoRef, id int, body string) error {
	a.id, a.body = id, body
	return nil
}

func TestGetChecksRefusesReusableStatusIdentity(t *testing.T) {
	api := &noticeAPI{statuses: []CommitStatus{{Name: "build", State: "SUCCESSFUL", UUID: "reused-uuid", URL: "https://ci.example/status/reused", UpdatedOn: "2025-01-01T00:00:00Z"}}}
	host := NewHost(api, RepoRef{Workspace: "test", RepoSlug: "repo"})
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatal(err)
	}
	if checks[0].AttemptID != "" {
		t.Fatalf("reusable Bitbucket status became attempt identity: %q", checks[0].AttemptID)
	}
	if _, err := host.GetCIAttemptIdentity(context.Background(), &scm.PR{Number: "123"}, "abc123", checks); err == nil || !strings.Contains(err.Error(), "build-status path does not expose") {
		t.Fatalf("GetCIAttemptIdentity() error = %v", err)
	}
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
