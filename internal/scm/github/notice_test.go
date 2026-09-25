package github

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/notices"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestListPRNoticesPaginatesExplicitly(t *testing.T) {
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --include --method GET repos/test/repo/issues/123/comments -f per_page=100 -f page=1": {
			stdout: "HTTP/2.0 200 OK\nLink: <next>; rel=\"next\"\n\n[{\"body\":\"first\"}]",
		},
		"gh api --include --method GET repos/test/repo/issues/123/comments -f per_page=100 -f page=2": {
			stdout: "HTTP/2.0 200 OK\n\n[{\"body\":\"second\"}]",
		},
	}), nil, "github.com", "test/repo")

	got, err := host.ListPRNotices(context.Background(), &scm.PR{Number: "123"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("notices = %#v", got)
	}
}

func TestListPRNoticesRefusesContinuationAtPageLimit(t *testing.T) {
	responses := make(map[string]githubTestResponse, notices.MaxPages)
	for page := 1; page <= notices.MaxPages; page++ {
		key := fmt.Sprintf("gh api --include --method GET repos/test/repo/issues/123/comments -f per_page=100 -f page=%d", page)
		responses[key] = githubTestResponse{stdout: "HTTP/2.0 200 OK\nLink: <next>; rel=\"next\"\n\n[]"}
	}
	host := New(githubTestCmdFactory(responses), nil, "github.com", "test/repo")
	_, err := host.ListPRNotices(context.Background(), &scm.PR{Number: "123"})
	if !errors.Is(err, notices.ErrCapacity) {
		t.Fatalf("error = %v, want capacity refusal", err)
	}
	t.Logf("PAGINATION-BOUNDED requests=%d continuation_refused=true", notices.MaxPages)
}

func TestPublishPRNoticeTargetsExactPR(t *testing.T) {
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr comment 123 --repo test/repo --body-file -": {},
	}), nil, "github.com", "test/repo")
	if err := host.PublishPRNotice(context.Background(), &scm.PR{Number: "123"}, "notice"); err != nil {
		t.Fatal(err)
	}
	if err := host.PublishPRNotice(context.Background(), &scm.PR{}, "notice"); err == nil {
		t.Fatal("missing PR identity did not fail closed")
	}
}

func TestValidationNoticeOperationsRefuseUnverifiedGitHubHosts(t *testing.T) {
	for _, hostName := range []string{"", "ghe.example.com"} {
		t.Run(fmt.Sprintf("host_%q", hostName), func(t *testing.T) {
			calls := 0
			factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
				calls++
				return exec.CommandContext(ctx, name, args...)
			}
			host := New(factory, nil, hostName, "test/repo")
			want := "verified only for github.com"
			if err := host.ValidateValidationNoticeSupport(); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("ValidateValidationNoticeSupport() error = %v", err)
			}
			if err := host.PublishPRNotice(context.Background(), &scm.PR{Number: "123"}, "notice"); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("PublishPRNotice() error = %v", err)
			}
			if _, err := host.ListPRNotices(context.Background(), &scm.PR{Number: "123"}); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("ListPRNotices() error = %v", err)
			}
			if _, err := host.GetCIAttemptIdentity(context.Background(), &scm.PR{Number: "123"}, "abc", []scm.Check{{Name: "build"}}); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("GetCIAttemptIdentity() error = %v", err)
			}
			if calls != 0 {
				t.Fatalf("unverified host executed %d remote commands", calls)
			}
		})
	}
}
