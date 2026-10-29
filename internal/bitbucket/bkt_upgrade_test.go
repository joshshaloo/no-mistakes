package bitbucket

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestBKTHostDraftCreationUsesBKTFlagAndVerifiesPR(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(fmt.Sprint(draft), func(t *testing.T) {
			client, fake := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
				switch {
				case commandIs(args, "pr", "create"):
					if slices.Contains(args, "--draft") != draft {
						t.Errorf("draft=%v args=%q", draft, args)
					}
					return fakeBKTResult{stdout: `{"id":42}`}
				case commandIs(args, "pr", "view"):
					return fakeBKTResult{stdout: `{"workspace":"ws","repo":"repo","pull_request":{"id":42,"state":"OPEN"}}`}
				default:
					t.Fatalf("unexpected command %q", args)
					return fakeBKTResult{}
				}
			})
			host := NewHost(client, RepoRef{Workspace: "ws", RepoSlug: "repo"}, draft)
			if err := host.Available(context.Background()); err != nil {
				t.Fatal(err)
			}
			pr, err := host.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: "title", Body: "body"})
			if err != nil || pr == nil || pr.Number != "42" {
				t.Fatalf("PR=%v err=%v", pr, err)
			}
			findCall(t, fake.snapshotCalls(), "pr", "view")
		})
	}
}

func TestBKTHostSelectedLogsRejectDifferentHead(t *testing.T) {
	client, fake := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
		if commandIs(args, "pr", "checks") {
			return fakeBKTResult{stdout: `{"workspace":"ws","repo":"repo","pull_request":42,"commit":"new-head","statuses":[{"name":"test","key":"linux","state":"FAILED","url":"https://bitbucket.org/ws/repo/pipelines/results/11111111-2222-3333-4444-555555555555"}]}`}
		}
		t.Fatalf("unexpected command %q", args)
		return fakeBKTResult{}
	})
	host := NewHost(client, RepoRef{Workspace: "ws", RepoSlug: "repo"})
	if err := host.Available(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "42"}, "feature", "run-head", []scm.CheckTarget{{Name: "test", ProviderID: "bitbucket-status:linux"}})
	if !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("FetchFailedCheckTargetLogs() error = %v, want ErrHeadChanged", err)
	}
	calls := fake.snapshotCalls()
	findCall(t, calls, "pr", "checks")
	for _, args := range calls {
		if commandIs(args, "pipeline", "view") || commandIs(args, "pipeline", "logs") {
			t.Fatalf("commands = %q, want no pipeline access after head mismatch", calls)
		}
	}
}

func TestBKTHostSelectedLogsKeepProviderIdentityAndPipelineUUID(t *testing.T) {
	const selected = "{11111111-2222-3333-4444-555555555555}"
	const other = "{99999999-2222-3333-4444-555555555555}"
	const step = "{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}"
	client, _ := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
		switch {
		case commandIs(args, "pr", "checks"):
			return fakeBKTResult{stdout: fmt.Sprintf(`{"workspace":"ws","repo":"repo","pull_request":42,"commit":"sha","statuses":[{"name":"test","key":"linux","state":"FAILED","url":"https://bitbucket.org/ws/repo/pipelines/results/%s"},{"name":"test","key":"macos","state":"FAILED","url":"https://bitbucket.org/ws/repo/pipelines/results/%s"}]}`, other, selected)}
		case commandIs(args, "pipeline", "view"):
			if normalizePipelineUUID(args[2]) != normalizePipelineUUID(selected) {
				t.Fatalf("wrong selected pipeline %q", args)
			}
			return fakeBKTResult{stdout: fmt.Sprintf(`{"pipeline":{"uuid":%q},"steps":[{"uuid":%q,"state":{"result":{"name":"FAILED"}}}]}`, selected, step)}
		case commandIs(args, "pipeline", "logs"):
			if normalizePipelineUUID(args[2]) != normalizePipelineUUID(selected) || !containsSequence(args, []string{"--step", step}) {
				t.Fatalf("wrong pipeline/step %q", args)
			}
			return fakeBKTResult{stdout: "selected failure"}
		default:
			t.Fatalf("unexpected command %q (branch-only discovery is forbidden)", args)
			return fakeBKTResult{}
		}
	})
	host := NewHost(client, RepoRef{Workspace: "ws", RepoSlug: "repo"})
	if err := host.Available(context.Background()); err != nil {
		t.Fatal(err)
	}
	targets := []scm.CheckTarget{{Name: "test", ProviderID: "bitbucket-status:macos"}, {Name: "test", ProviderID: "bitbucket-status:missing"}}
	logs, err := host.FetchFailedCheckTargetLogs(context.Background(), &scm.PR{Number: "42"}, "feature", "sha", targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 || logs[0].Output != "selected failure" || logs[0].Err != nil || logs[1].Err == nil || logs[1].Output != "" {
		t.Fatalf("selected logs=%+v", logs)
	}
}
