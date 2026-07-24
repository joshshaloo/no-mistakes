package bitbucket

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const fakeSecret = "ATBB_secret-shaped-fixture-value"

type fakeBKTResult struct {
	stdout string
	stderr string
	exit   int
	delay  time.Duration
	bytes  int
}

type fakeBKT struct {
	t         *testing.T
	mu        sync.Mutex
	calls     [][]string
	responder func([]string) fakeBKTResult
}

func (f *fakeBKT) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	f.t.Helper()
	if name != "bkt" {
		f.t.Fatalf("command name = %q, want bkt", name)
	}
	copied := append([]string(nil), args...)
	f.mu.Lock()
	f.calls = append(f.calls, copied)
	f.mu.Unlock()

	result := f.responder(copied)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestBKTCommandHelper", "--")
	cmd.Env = append(os.Environ(),
		"GO_WANT_BKT_COMMAND_HELPER=1",
		"BKT_FAKE_STDOUT="+base64.StdEncoding.EncodeToString([]byte(result.stdout)),
		"BKT_FAKE_STDERR="+base64.StdEncoding.EncodeToString([]byte(result.stderr)),
		"BKT_FAKE_EXIT="+strconv.Itoa(result.exit),
		"BKT_FAKE_DELAY="+result.delay.String(),
		"BKT_FAKE_BYTES="+strconv.Itoa(result.bytes),
	)
	return cmd
}

func (f *fakeBKT) snapshotCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i := range f.calls {
		out[i] = append([]string(nil), f.calls[i]...)
	}
	return out
}

func TestBKTCommandHelper(t *testing.T) {
	if os.Getenv("GO_WANT_BKT_COMMAND_HELPER") != "1" {
		return
	}
	if delay, err := time.ParseDuration(os.Getenv("BKT_FAKE_DELAY")); err == nil && delay > 0 {
		time.Sleep(delay)
	}
	if n, _ := strconv.Atoi(os.Getenv("BKT_FAKE_BYTES")); n > 0 {
		chunk := strings.Repeat("x", 4096)
		for n > 0 {
			write := min(n, len(chunk))
			_, _ = os.Stdout.WriteString(chunk[:write])
			n -= write
		}
	}
	if encoded := os.Getenv("BKT_FAKE_STDOUT"); encoded != "" {
		decoded, _ := base64.StdEncoding.DecodeString(encoded)
		_, _ = os.Stdout.Write(decoded)
	}
	if encoded := os.Getenv("BKT_FAKE_STDERR"); encoded != "" {
		decoded, _ := base64.StdEncoding.DecodeString(encoded)
		_, _ = os.Stderr.Write(decoded)
	}
	if code, _ := strconv.Atoi(os.Getenv("BKT_FAKE_EXIT")); code != 0 {
		os.Exit(code)
	}
	os.Exit(0)
}

func healthyBKTResponse(args []string) (fakeBKTResult, bool) {
	switch {
	case slices.Equal(args, []string{"--version"}):
		return fakeBKTResult{stdout: "bkt version 0.30.0\n"}, true
	case slices.Equal(args, []string{"auth", "status", "--json"}):
		return fakeBKTResult{stdout: healthyBKTAuthJSON()}, true
	default:
		return fakeBKTResult{}, false
	}
}

func healthyBKTAuthJSON() string {
	return `{
		"active_context":"work",
		"hosts":[{"key":"api.bitbucket.org","kind":"cloud","base_url":"https://api.bitbucket.org/2.0","username":"dev@example.com","auth_method":"basic","token_source":"keyring"}],
		"contexts":[{"name":"work","host":"api.bitbucket.org","workspace":"other-workspace","active":true}]
	}`
}

func newHealthyFakeBKT(t *testing.T, responder func([]string) fakeBKTResult) (*BKTClient, *fakeBKT) {
	t.Helper()
	fake := &fakeBKT{t: t}
	fake.responder = func(args []string) fakeBKTResult {
		if result, ok := healthyBKTResponse(args); ok {
			return result
		}
		return responder(args)
	}
	return NewBKTClient(fake.factory), fake
}

func TestParseRepoRefForBKTSelectors(t *testing.T) {
	for _, raw := range []string{
		"https://bitbucket.org/envy-forge/app.git",
		"git@bitbucket.org:envy-forge/app.git",
		"ssh://git@bitbucket.org/envy-forge/app.git",
		"https://bitbucket.org/envy-forge/app/pull-requests/42",
	} {
		repo, err := ParseRepoRef(raw)
		if err != nil {
			t.Fatalf("ParseRepoRef(%q) error = %v", raw, err)
		}
		if repo != (RepoRef{Workspace: "envy-forge", RepoSlug: "app"}) {
			t.Fatalf("ParseRepoRef(%q) = %#v", raw, repo)
		}
	}
}

func TestNewAPIFromEnvOrBKTSelectionPrecedence(t *testing.T) {
	t.Setenv(envEmail, "")
	t.Setenv(envToken, "")

	factory := func(context.Context, string, ...string) *exec.Cmd {
		t.Fatal("factory must not run while resolving a client")
		return nil
	}

	api, err := NewAPIFromEnvOrBKT([]string{
		envEmail + "=direct@example.com",
		envToken + "=" + fakeSecret,
	}, factory)
	if err != nil {
		t.Fatalf("direct resolution error = %v", err)
	}
	if _, ok := api.(*Client); !ok {
		t.Fatalf("both direct credentials selected %T, want *Client", api)
	}

	api, err = NewAPIFromEnvOrBKT([]string{envEmail + "=", envToken + "="}, factory)
	if err != nil {
		t.Fatalf("bkt resolution error = %v", err)
	}
	if _, ok := api.(*BKTClient); !ok {
		t.Fatalf("absent direct credentials selected %T, want *BKTClient", api)
	}

	_, err = NewAPIFromEnvOrBKT([]string{envEmail + "=direct@example.com", envToken + "="}, factory)
	if err == nil || !strings.Contains(err.Error(), envToken) {
		t.Fatalf("partial direct credentials error = %v, want explicit missing %s", err, envToken)
	}
}

func TestBKTClientAvailableRequiresCompatibleAuthenticatedCloudContext(t *testing.T) {
	tests := []struct {
		name      string
		version   fakeBKTResult
		auth      fakeBKTResult
		wantError string
	}{
		{
			name:      "unavailable",
			version:   fakeBKTResult{stderr: fakeSecret, exit: 127},
			wantError: "version check failed",
		},
		{
			name:      "old version",
			version:   fakeBKTResult{stdout: "bkt version 0.29.9\n"},
			wantError: "requires bkt 0.30.0 or newer",
		},
		{
			name:      "unauthenticated",
			version:   fakeBKTResult{stdout: "bkt version 0.30.0\n"},
			auth:      fakeBKTResult{stderr: fakeSecret, exit: 1},
			wantError: "authentication check failed",
		},
		{
			name:    "data center context",
			version: fakeBKTResult{stdout: "bkt version 0.30.0\n"},
			auth: fakeBKTResult{stdout: `{
				"active_context":"dc",
				"hosts":[{"key":"stash.example.com","kind":"dc","base_url":"https://stash.example.com","username":"dev","token_source":"keyring"}],
				"contexts":[{"name":"dc","host":"stash.example.com","active":true}]
			}`},
			wantError: "no authenticated Bitbucket Cloud Keychain context",
		},
		{
			name:    "mismatched context host",
			version: fakeBKTResult{stdout: "bkt version 0.30.0\n"},
			auth: fakeBKTResult{stdout: `{
				"active_context":"wrong",
				"hosts":[{"key":"api.bitbucket.org","kind":"cloud","base_url":"https://api.bitbucket.org/2.0","username":"dev@example.com","token_source":"keyring"}],
				"contexts":[{"name":"wrong","host":"evil.example","workspace":"ws","active":true}]
			}`},
			wantError: "no authenticated Bitbucket Cloud Keychain context",
		},
		{
			name:    "ambient token source is rejected",
			version: fakeBKTResult{stdout: "bkt version 0.30.0\n"},
			auth: fakeBKTResult{stdout: `{
				"active_context":"cloud",
				"hosts":[{"key":"api.bitbucket.org","kind":"cloud","base_url":"https://api.bitbucket.org/2.0","username":"dev@example.com","token_source":"env"}],
				"contexts":[{"name":"cloud","host":"api.bitbucket.org","workspace":"ws","active":true}]
			}`},
			wantError: "no authenticated Bitbucket Cloud Keychain context",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeBKT{t: t}
			fake.responder = func(args []string) fakeBKTResult {
				switch {
				case slices.Equal(args, []string{"--version"}):
					return tt.version
				case slices.Equal(args, []string{"auth", "status", "--json"}):
					return tt.auth
				default:
					t.Fatalf("unexpected bkt args: %q", args)
					return fakeBKTResult{}
				}
			}
			client := NewBKTClient(fake.factory)
			err := client.Available(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Available() error = %v, want substring %q", err, tt.wantError)
			}
			if strings.Contains(err.Error(), fakeSecret) {
				t.Fatalf("Available() leaked command output secret: %v", err)
			}
		})
	}
}

func TestBKTClientPRDiscoveryCreateAndUpdateUseExplicitSelectors(t *testing.T) {
	repo := RepoRef{Workspace: "envy-forge", RepoSlug: "app"}
	view := `{"workspace":"envy-forge","repo":"app","pull_request":{"id":42,"state":"OPEN","source":{"branch":{"name":"feature"},"commit":{"hash":"abc123"},"repository":{"full_name":"envy-forge/app"}},"destination":{"branch":{"name":"main"}},"links":{"html":{"href":"https://bitbucket.org/envy-forge/app/pull-requests/42"}}}}`
	client, fake := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
		switch {
		case commandIs(args, "pr", "list"):
			return fakeBKTResult{stdout: `{"workspace":"envy-forge","repo":"app","pull_requests":[{"id":11,"state":"OPEN","source":{"branch":{"name":"other"},"repository":{"full_name":"envy-forge/app"}},"destination":{"branch":{"name":"main"}}},{"id":42,"state":"OPEN","source":{"branch":{"name":"feature"},"commit":{"hash":"abc123"},"repository":{"full_name":"envy-forge/app"}},"destination":{"branch":{"name":"main"}},"links":{"html":{"href":"https://bitbucket.org/envy-forge/app/pull-requests/42"}}}]}`}
		case commandIs(args, "pr", "create"):
			return fakeBKTResult{stdout: `{"id":42,"title":"feat: ship","url":"https://bitbucket.org/envy-forge/app/pull-requests/42"}`}
		case commandIs(args, "pr", "edit"):
			return fakeBKTResult{stdout: view}
		case commandIs(args, "pr", "view"):
			return fakeBKTResult{stdout: view}
		default:
			t.Fatalf("unexpected bkt args: %q", args)
			return fakeBKTResult{}
		}
	})
	if err := client.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}

	found, err := client.FindOpenPRBySourceBranch(context.Background(), repo, "feature", "main")
	if err != nil {
		t.Fatalf("FindOpenPRBySourceBranch() error = %v", err)
	}
	if found == nil || found.ID != 42 || found.SourceCommitHash != "abc123" {
		t.Fatalf("found PR = %#v, want #42 at abc123", found)
	}

	created, err := client.CreatePR(context.Background(), repo, "feature", "main", "feat: ship", "body")
	if err != nil || created == nil || created.ID != 42 {
		t.Fatalf("CreatePR() = %#v, %v", created, err)
	}
	updated, err := client.UpdatePR(context.Background(), repo, 42, "feat: shipped", "updated body")
	if err != nil || updated == nil || updated.ID != 42 {
		t.Fatalf("UpdatePR() = %#v, %v", updated, err)
	}

	for _, args := range fake.snapshotCalls() {
		if slices.Equal(args, []string{"--version"}) || slices.Equal(args, []string{"auth", "status", "--json"}) {
			continue
		}
		for _, want := range [][]string{{"--context", "work"}, {"--workspace", "envy-forge"}, {"--repo", "app"}, {"--json"}} {
			if !containsSequence(args, want) {
				t.Errorf("args %q missing explicit selector %q", args, want)
			}
		}
	}
	createArgs := findCall(t, fake.snapshotCalls(), "pr", "create")
	for _, want := range [][]string{{"--source", "feature"}, {"--target", "main"}} {
		if !containsSequence(createArgs, want) {
			t.Errorf("create args %q missing %q", createArgs, want)
		}
	}
}

func TestBKTClientRejectsMismatchedRepositoryEnvelope(t *testing.T) {
	client, _ := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
		if commandIs(args, "pr", "list") {
			return fakeBKTResult{stdout: `{"workspace":"someone-else","repo":"app","pull_requests":[]}`}
		}
		t.Fatalf("unexpected bkt args: %q", args)
		return fakeBKTResult{}
	})
	if err := client.Available(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := client.FindOpenPRBySourceBranch(context.Background(), RepoRef{Workspace: "envy-forge", RepoSlug: "app"}, "feature", "main")
	if err == nil || !strings.Contains(err.Error(), "mismatched repository") {
		t.Fatalf("error = %v, want mismatched repository", err)
	}
}

func TestBKTHostReadsTerminalPRStatesAndChecks(t *testing.T) {
	repo := RepoRef{Workspace: "envy-forge", RepoSlug: "app"}
	states := []struct {
		raw  string
		want scm.PRState
	}{{"OPEN", scm.PRStateOpen}, {"MERGED", scm.PRStateMerged}, {"DECLINED", scm.PRStateClosed}}
	for _, tt := range states {
		t.Run(tt.raw, func(t *testing.T) {
			client, _ := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
				switch {
				case commandIs(args, "pr", "view"):
					return fakeBKTResult{stdout: fmt.Sprintf(`{"workspace":"envy-forge","repo":"app","pull_request":{"id":42,"state":%q,"source":{"commit":{"hash":"abc123"}},"links":{"html":{"href":"https://bitbucket.org/envy-forge/app/pull-requests/42"}}}}`, tt.raw)}
				case commandIs(args, "pr", "checks"):
					return fakeBKTResult{stdout: `{"workspace":"envy-forge","repo":"app","pull_request":42,"commit":"abc123","statuses":[{"name":"pass","key":"pass","state":"SUCCESSFUL"},{"name":"fail","key":"fail","state":"FAILED"},{"name":"pending","key":"pending","state":"INPROGRESS"}]}`}
				default:
					t.Fatalf("unexpected bkt args: %q", args)
					return fakeBKTResult{}
				}
			})
			if err := client.Available(context.Background()); err != nil {
				t.Fatal(err)
			}
			host := NewHost(client, repo)
			pr := &scm.PR{Number: "42", URL: "https://bitbucket.org/envy-forge/app/pull-requests/42"}
			state, err := host.GetPRState(context.Background(), pr)
			if err != nil || state != tt.want {
				t.Fatalf("GetPRState() = %q, %v; want %q", state, err, tt.want)
			}
			checks, err := host.GetChecks(context.Background(), pr)
			if err != nil {
				t.Fatal(err)
			}
			want := []scm.CheckBucket{scm.CheckBucketPass, scm.CheckBucketFail, scm.CheckBucketPending}
			if len(checks) != len(want) {
				t.Fatalf("checks = %#v", checks)
			}
			for i := range want {
				if checks[i].Bucket != want[i] {
					t.Errorf("checks[%d].Bucket = %q, want %q", i, checks[i].Bucket, want[i])
				}
			}
		})
	}
}

func TestBKTHostFetchesBoundedFailedPipelineStepLogs(t *testing.T) {
	repo := RepoRef{Workspace: "envy-forge", RepoSlug: "app"}
	pipelineID := "{11111111-2222-3333-4444-555555555555}"
	stepID := "{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}"
	client, fake := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
		switch {
		case commandIs(args, "pr", "checks"):
			return fakeBKTResult{stdout: fmt.Sprintf(`{"workspace":"envy-forge","repo":"app","pull_request":42,"commit":"abc123","statuses":[{"name":"test","key":"test","state":"FAILED","url":"https://bitbucket.org/envy-forge/app/pipelines/results/%s"}]}`, pipelineID)}
		case commandIs(args, "pipeline", "view"):
			return fakeBKTResult{stdout: fmt.Sprintf(`{"pipeline":{"uuid":%q,"build_number":9,"state":{"name":"COMPLETED","result":{"name":"FAILED"}},"target":{"ref":{"name":"feature"}}},"steps":[{"uuid":%q,"name":"test","state":{"name":"COMPLETED","result":{"name":"FAILED"}},"result":{"name":"FAILED"}}]}`, pipelineID, stepID)}
		case commandIs(args, "pipeline", "logs"):
			return fakeBKTResult{bytes: maxStepLogBytes + 1024, stdout: "\nFAILED-LOG-TAIL\n"}
		default:
			t.Fatalf("unexpected bkt args: %q", args)
			return fakeBKTResult{}
		}
	})
	if err := client.Available(context.Background()); err != nil {
		t.Fatal(err)
	}
	host := NewHost(client, repo)
	logs, err := host.FetchFailedCheckLogs(context.Background(), &scm.PR{Number: "42"}, "feature", "abc123", []string{"test"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if !strings.HasSuffix(logs, "FAILED-LOG-TAIL") {
		t.Fatalf("logs tail = %q", logs)
	}
	if len(logs) > maxStepLogBytes {
		t.Fatalf("logs length = %d, want <= %d", len(logs), maxStepLogBytes)
	}
	logArgs := findCall(t, fake.snapshotCalls(), "pipeline", "logs")
	if !containsSequence(logArgs, []string{"--step", stepID}) {
		t.Fatalf("log args %q missing failed step selector", logArgs)
	}
	if !slices.Contains(logArgs, "--json") {
		t.Fatalf("log args %q missing structured-output flag", logArgs)
	}
}

func TestBKTClientMalformedJSONTimeoutCancellationAndBoundedOutput(t *testing.T) {
	repo := RepoRef{Workspace: "envy-forge", RepoSlug: "app"}
	t.Run("malformed json does not leak output", func(t *testing.T) {
		client, _ := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
			return fakeBKTResult{stdout: "not-json-" + fakeSecret}
		})
		if err := client.Available(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, err := client.GetPR(context.Background(), repo, 42)
		if err == nil || !strings.Contains(err.Error(), "decode JSON") {
			t.Fatalf("error = %v, want decode JSON", err)
		}
		if strings.Contains(err.Error(), fakeSecret) {
			t.Fatalf("malformed JSON error leaked output: %v", err)
		}
	})

	t.Run("caller timeout", func(t *testing.T) {
		client, _ := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
			return fakeBKTResult{delay: time.Second, stdout: `{}`}
		})
		if err := client.Available(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		_, err := client.GetPR(ctx, repo, 42)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context deadline exceeded", err)
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		client, _ := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
			return fakeBKTResult{delay: time.Second, stdout: `{}`}
		})
		if err := client.Available(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.GetPR(ctx, repo, 42)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	})

	t.Run("oversized JSON is rejected without retention", func(t *testing.T) {
		client, _ := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
			return fakeBKTResult{bytes: maxBKTJSONBytes + 1}
		})
		if err := client.Available(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, err := client.GetPR(context.Background(), repo, 42)
		if err == nil || !strings.Contains(err.Error(), "output exceeded") {
			t.Fatalf("error = %v, want bounded-output error", err)
		}
	})

	t.Run("failed command surfaces sanitized stderr without secret arguments", func(t *testing.T) {
		client, fake := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
			return fakeBKTResult{stderr: "request rejected: " + fakeSecret, exit: 1}
		})
		if err := client.Available(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, err := client.GetPR(context.Background(), repo, 42)
		if err == nil {
			t.Fatal("expected command error")
		}
		if strings.Contains(err.Error(), fakeSecret) {
			t.Fatalf("command error leaked stderr: %v", err)
		}
		if !strings.Contains(err.Error(), "request rejected") {
			t.Fatalf("command error dropped the stderr diagnostic: %v", err)
		}
		if strings.Contains(fmt.Sprint(fake.snapshotCalls()), fakeSecret) {
			t.Fatalf("command arguments leaked fixture secret: %q", fake.snapshotCalls())
		}
	})
}

func TestBKTClientFailedCommandStderrIsSanitizedAndBounded(t *testing.T) {
	repo := RepoRef{Workspace: "envy-forge", RepoSlug: "app"}
	failWith := func(t *testing.T, stderr string) error {
		t.Helper()
		client, _ := newHealthyFakeBKT(t, func(args []string) fakeBKTResult {
			return fakeBKTResult{stderr: stderr, exit: 1}
		})
		if err := client.Available(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, err := client.GetPR(context.Background(), repo, 42)
		if err == nil {
			t.Fatal("expected command error")
		}
		return err
	}

	t.Run("credentialed URL keeps target but redacts userinfo", func(t *testing.T) {
		err := failWith(t, "fetch https://x-token-auth:"+fakeSecret+"@bitbucket.org/envy-forge/app.git failed")
		if strings.Contains(err.Error(), fakeSecret) || strings.Contains(err.Error(), "x-token-auth") {
			t.Fatalf("error leaked URL credential: %v", err)
		}
		if !strings.Contains(err.Error(), "redacted@bitbucket.org/envy-forge/app.git") {
			t.Fatalf("error dropped the redacted URL diagnostic: %v", err)
		}
	})

	t.Run("standalone token is redacted while scope diagnostics survive", func(t *testing.T) {
		err := failWith(t, "403 Forbidden: Your credentials lack one or more required privilege scopes ("+fakeSecret+")")
		if strings.Contains(err.Error(), fakeSecret) {
			t.Fatalf("error leaked standalone token: %v", err)
		}
		for _, want := range []string{"403 Forbidden", "privilege scopes", "[redacted]"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want substring %q", err, want)
			}
		}
	})

	t.Run("credential assignments and auth headers are redacted", func(t *testing.T) {
		err := failWith(t, "request rejected: api_token=plain-secret-value Authorization: Bearer opaque-secret-892f31 scope=repository:write")
		for _, leaked := range []string{"plain-secret-value", "opaque-secret-892f31"} {
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("error leaked %q: %v", leaked, err)
			}
		}
		for _, want := range []string{"request rejected", "scope=repository:write"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want substring %q", err, want)
			}
		}
	})

	t.Run("malformed stderr yields valid UTF-8 without secrets", func(t *testing.T) {
		err := failWith(t, "scope \xff\xfe denied "+fakeSecret)
		if !utf8.ValidString(err.Error()) {
			t.Fatalf("error is not valid UTF-8: %q", err.Error())
		}
		if strings.Contains(err.Error(), fakeSecret) {
			t.Fatalf("error leaked secret from malformed stderr: %v", err)
		}
		if !strings.Contains(err.Error(), "denied") {
			t.Fatalf("error dropped the diagnostic: %v", err)
		}
	})

	t.Run("oversized stderr stays bounded and marked truncated", func(t *testing.T) {
		err := failWith(t, "scope failure: "+strings.Repeat("x", maxBKTStderrBytes)+fakeSecret)
		if strings.Contains(err.Error(), fakeSecret) {
			t.Fatalf("error leaked overflow bytes: %v", err)
		}
		for _, want := range []string{"scope failure", "[stderr truncated]"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %.120q..., want substring %q", err.Error(), want)
			}
		}
		if len(err.Error()) > maxBKTStderrBytes+128 {
			t.Fatalf("error length = %d, want <= %d", len(err.Error()), maxBKTStderrBytes+128)
		}
	})
}

func commandIs(args []string, group, action string) bool {
	return len(args) >= 2 && args[0] == group && args[1] == action
}

func containsSequence(values, sequence []string) bool {
	if len(sequence) == 0 {
		return true
	}
	for i := 0; i+len(sequence) <= len(values); i++ {
		if slices.Equal(values[i:i+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func findCall(t *testing.T, calls [][]string, group, action string) []string {
	t.Helper()
	for _, args := range calls {
		if commandIs(args, group, action) {
			return args
		}
	}
	t.Fatalf("no %s %s call in %q", group, action, calls)
	return nil
}
