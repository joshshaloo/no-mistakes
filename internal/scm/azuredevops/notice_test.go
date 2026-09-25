package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/notices"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPublishPRNoticeCreatesAppendOnlyThreadForExactPR(t *testing.T) {
	var gotArgs []string
	var inputPath string
	var inputBody []byte
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "az" {
			t.Fatalf("command = %q, want az", name)
		}
		gotArgs = append([]string(nil), args...)
		for i, arg := range args {
			if arg == "--in-file" && i+1 < len(args) {
				inputPath = args[i+1]
				var err error
				inputBody, err = os.ReadFile(inputPath)
				if err != nil {
					t.Fatalf("read notice input: %v", err)
				}
			}
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestAzdoHelperProcess", "--")
		cmd.Env = append(os.Environ(), "AZDO_TEST_HELPER=1")
		return cmd
	}
	host := New(factory, func() bool { return true }, testOrg, testProject, testRepo)
	body := "head-bound notice\nfor exact head"
	if err := host.PublishPRNotice(context.Background(), &scm.PR{Number: "123"}, body); err != nil {
		t.Fatal(err)
	}

	if inputPath == "" {
		t.Fatal("az devops invoke did not receive --in-file")
	}
	if _, err := os.Stat(inputPath); !os.IsNotExist(err) {
		t.Fatalf("notice input file remained after publication: %v", err)
	}
	wantArgs := []string{
		"devops", "invoke",
		"--area", "git",
		"--resource", "pullRequestThreads",
		"--route-parameters", "project=" + testProject, "repositoryId=" + testRepo, "pullRequestId=123",
		"--http-method", "POST",
		"--api-version", "7.1",
		"--in-file", inputPath,
		"--organization", testOrg,
		"--output", "none",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args = %#v, want %#v", gotArgs, wantArgs)
	}
	var payload struct {
		Comments []struct {
			ParentCommentID int    `json:"parentCommentId"`
			Content         string `json:"content"`
			CommentType     int    `json:"commentType"`
		} `json:"comments"`
		Status int `json:"status"`
	}
	if err := json.Unmarshal(inputBody, &payload); err != nil {
		t.Fatalf("notice input is not JSON: %v\n%s", err, inputBody)
	}
	if len(payload.Comments) != 1 || payload.Comments[0].Content != body || payload.Comments[0].ParentCommentID != 0 || payload.Comments[0].CommentType != 1 || payload.Status != 1 {
		t.Fatalf("notice payload = %+v, want one active text thread containing exact body", payload)
	}
}

func TestListPRNoticesRequiresCompleteAllThreadsEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    []string
		wantErr string
	}{
		{name: "empty", output: `{"count":0,"value":[]}`, want: []string{}},
		{name: "multiple threads", output: `{"count":2,"value":[{"comments":[{"content":"one"},{"content":"two"}]},{"comments":[{"content":"three"}]}]}`, want: []string{"one", "two", "three"}},
		{name: "missing value", output: `{"count":0}`, wantErr: "missing value"},
		{name: "count mismatch", output: `{"count":2,"value":[{"comments":[]}]}`, wantErr: "thread count"},
		{name: "unexpected continuation", output: `{"count":0,"value":[],"continuationToken":"more"}`, wantErr: "unexpected field"},
		{name: "malformed", output: `{"count":`, wantErr: "parse Azure PR threads"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			factory := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				calls++
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestAzdoHelperProcess", "--")
				cmd.Env = append(os.Environ(), "AZDO_TEST_HELPER=1", "AZDO_TEST_STDOUT="+tt.output)
				return cmd
			}
			host := New(factory, func() bool { return true }, testOrg, testProject, testRepo)
			got, err := host.ListPRNotices(context.Background(), &scm.PR{Number: "123"})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("notices = %#v, want %#v", got, tt.want)
			}
			if calls != 1 {
				t.Fatalf("provider calls = %d, want one all-threads request", calls)
			}
		})
	}

	t.Run("over budget", func(t *testing.T) {
		factory := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestAzdoHelperProcess", "--")
			cmd.Env = append(os.Environ(), "AZDO_TEST_HELPER=1", fmt.Sprintf("AZDO_TEST_STDOUT_BYTES=%d", notices.MaxResponseBytes+1))
			return cmd
		}
		host := New(factory, func() bool { return true }, testOrg, testProject, testRepo)
		_, err := host.ListPRNotices(context.Background(), &scm.PR{Number: "123"})
		if !errors.Is(err, notices.ErrCapacity) {
			t.Fatalf("error = %v, want capacity refusal", err)
		}
	})
}

func TestPublishPRNoticeFailsClosed(t *testing.T) {
	t.Run("missing identity", func(t *testing.T) {
		host := newTestHost(nil)
		if err := host.PublishPRNotice(context.Background(), &scm.PR{}, "notice"); err == nil {
			t.Fatal("missing PR identity did not fail closed")
		}
	})

	t.Run("provider error", func(t *testing.T) {
		factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestAzdoHelperProcess", "--")
			cmd.Env = append(os.Environ(),
				"AZDO_TEST_HELPER=1",
				"AZDO_TEST_STDERR=thread rejected",
				"AZDO_TEST_EXIT_CODE=1",
			)
			return cmd
		}
		host := New(factory, func() bool { return true }, testOrg, testProject, testRepo)
		err := host.PublishPRNotice(context.Background(), &scm.PR{Number: "123"}, "notice")
		if err == nil || !strings.Contains(err.Error(), "thread rejected") {
			t.Fatalf("PublishPRNotice() error = %v, want provider failure", err)
		}
	})

	t.Run("missing route scope", func(t *testing.T) {
		host := New(func(context.Context, string, ...string) *exec.Cmd {
			t.Fatal("provider command ran without complete route scope")
			return nil
		}, func() bool { return true }, testOrg, "", testRepo)
		err := host.PublishPRNotice(context.Background(), &scm.PR{Number: "123"}, "notice")
		if err == nil || !strings.Contains(err.Error(), "missing project or repository") {
			t.Fatalf("PublishPRNotice() error = %v, want route-scope failure", err)
		}
	})
}
