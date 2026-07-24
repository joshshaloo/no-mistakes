package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const (
	minimumBKTVersion  = "0.30.0"
	maxBKTJSONBytes    = 2 * 1024 * 1024
	maxBKTStderrBytes  = 32 * 1024
	maxBKTVersionBytes = 4 * 1024
	bktCommandTimeout  = 30 * time.Second
	bktProbeTimeout    = 10 * time.Second
)

// BKTClient invokes bkt directly and leaves authentication entirely under
// bkt's ownership. It never reads, exports, or passes a Bitbucket token.
type BKTClient struct {
	factory CommandFactory

	mu          sync.RWMutex
	contextName string
}

func NewBKTClient(factory CommandFactory) *BKTClient {
	if factory == nil {
		factory = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		}
	}
	return &BKTClient{factory: factory}
}

type bktAuthStatus struct {
	ActiveContext string `json:"active_context"`
	Hosts         []struct {
		Key         string `json:"key"`
		Kind        string `json:"kind"`
		BaseURL     string `json:"base_url"`
		Username    string `json:"username"`
		TokenSource string `json:"token_source"`
	} `json:"hosts"`
	Contexts []struct {
		Name      string `json:"name"`
		Host      string `json:"host"`
		Workspace string `json:"workspace"`
		Active    bool   `json:"active"`
	} `json:"contexts"`
}

func (c *BKTClient) Available(ctx context.Context) error {
	versionOutput, err := c.runPrefix(ctx, bktProbeTimeout, "version check", maxBKTVersionBytes, "--version")
	if err != nil {
		return err
	}
	version, ok := parseBKTVersion(string(versionOutput))
	if !ok || compareVersions(version, minimumBKTVersion) < 0 {
		return fmt.Errorf("Bitbucket Cloud via bkt requires bkt %s or newer", minimumBKTVersion)
	}

	var status bktAuthStatus
	if err := c.runJSONWithTimeout(ctx, bktProbeTimeout, "authentication check", &status, "auth", "status", "--json"); err != nil {
		return err
	}
	contextName := compatibleCloudContext(status)
	if contextName == "" {
		return errors.New("no authenticated Bitbucket Cloud Keychain context is configured in bkt")
	}
	c.mu.Lock()
	c.contextName = contextName
	c.mu.Unlock()
	return nil
}

func parseBKTVersion(raw string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(raw))
	for i, field := range fields {
		if strings.EqualFold(field, "version") && i+1 < len(fields) {
			candidate := strings.TrimPrefix(strings.TrimSpace(fields[i+1]), "v")
			if validVersion(candidate) {
				return candidate, true
			}
		}
	}
	return "", false
}

func validVersion(raw string) bool {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

func compareVersions(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		av, _ := strconv.Atoi(aParts[i])
		bv, _ := strconv.Atoi(bParts[i])
		switch {
		case av < bv:
			return -1
		case av > bv:
			return 1
		}
	}
	return 0
}

func compatibleCloudContext(status bktAuthStatus) string {
	compatibleHosts := make(map[string]struct{})
	for _, host := range status.Hosts {
		if !strings.EqualFold(strings.TrimSpace(host.Kind), "cloud") ||
			!strings.EqualFold(strings.TrimSpace(host.TokenSource), "keyring") ||
			strings.TrimSpace(host.Username) == "" {
			continue
		}
		keyHost := canonicalHost(host.Key)
		baseHost := canonicalHost(host.BaseURL)
		if !isBitbucketCloudHost(keyHost) || !isBitbucketCloudHost(baseHost) {
			continue
		}
		compatibleHosts[keyHost] = struct{}{}
		compatibleHosts[baseHost] = struct{}{}
	}

	var names []string
	active := ""
	for _, configuredContext := range status.Contexts {
		name := strings.TrimSpace(configuredContext.Name)
		host := canonicalHost(configuredContext.Host)
		if name == "" {
			continue
		}
		if _, ok := compatibleHosts[host]; !ok {
			continue
		}
		names = append(names, name)
		if configuredContext.Active || name == status.ActiveContext {
			active = name
		}
	}
	if active != "" {
		return active
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

func canonicalHost(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

func isBitbucketCloudHost(host string) bool {
	return host == "bitbucket.org" || host == "api.bitbucket.org"
}

func (c *BKTClient) commandContext() (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if strings.TrimSpace(c.contextName) == "" {
		return "", errors.New("bkt availability has not been verified")
	}
	return c.contextName, nil
}

func (c *BKTClient) repoArgs(repo RepoRef) ([]string, error) {
	contextName, err := c.commandContext()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(repo.Workspace) == "" || strings.TrimSpace(repo.RepoSlug) == "" {
		return nil, errors.New("Bitbucket workspace and repository are required")
	}
	return []string{
		"--context", contextName,
		"--workspace", repo.Workspace,
		"--repo", repo.RepoSlug,
	}, nil
}

type bktPullRequest struct {
	ID     int    `json:"id"`
	State  string `json:"state"`
	Source struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
		Commit struct {
			Hash string `json:"hash"`
		} `json:"commit"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	} `json:"source"`
	Destination struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"destination"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

type bktPRListEnvelope struct {
	Workspace    string           `json:"workspace"`
	Repo         string           `json:"repo"`
	PullRequests []bktPullRequest `json:"pull_requests"`
}

type bktPRViewEnvelope struct {
	Workspace   string         `json:"workspace"`
	Repo        string         `json:"repo"`
	PullRequest bktPullRequest `json:"pull_request"`
}

func (c *BKTClient) FindOpenPRBySourceBranch(ctx context.Context, repo RepoRef, branch, destBranch string) (*PullRequest, error) {
	selectors, err := c.repoArgs(repo)
	if err != nil {
		return nil, err
	}
	args := []string{"pr", "list"}
	args = append(args, selectors...)
	args = append(args, "--state", "OPEN", "--limit", "0", "--json")
	var envelope bktPRListEnvelope
	if err := c.runJSON(ctx, "PR discovery", &envelope, args...); err != nil {
		return nil, err
	}
	if err := validateRepoEnvelope(repo, envelope.Workspace, envelope.Repo); err != nil {
		return nil, err
	}
	fullName := repo.Workspace + "/" + repo.RepoSlug
	for _, candidate := range envelope.PullRequests {
		if !strings.EqualFold(strings.TrimSpace(candidate.State), "OPEN") || candidate.Source.Branch.Name != branch {
			continue
		}
		if strings.TrimSpace(destBranch) != "" && candidate.Destination.Branch.Name != destBranch {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(candidate.Source.Repository.FullName), fullName) {
			continue
		}
		return bktPRToPullRequest(repo, candidate)
	}
	return nil, nil
}

func (c *BKTClient) CreatePR(ctx context.Context, repo RepoRef, sourceBranch, destBranch, title, body string) (*PullRequest, error) {
	selectors, err := c.repoArgs(repo)
	if err != nil {
		return nil, err
	}
	args := []string{"pr", "create"}
	args = append(args, selectors...)
	args = append(args,
		"--source", sourceBranch,
		"--target", destBranch,
		"--title", title,
		"--body", body,
		"--json",
	)
	var result struct {
		ID int `json:"id"`
	}
	if err := c.runJSON(ctx, "PR creation", &result, args...); err != nil {
		return nil, err
	}
	if result.ID <= 0 {
		return nil, errors.New("bkt PR creation returned no pull request ID")
	}
	// Verify the mutation with an independently targeted read rather than
	// trusting a successful process exit or the create response alone.
	return c.GetPR(ctx, repo, result.ID)
}

func (c *BKTClient) UpdatePR(ctx context.Context, repo RepoRef, prID int, title, body string) (*PullRequest, error) {
	selectors, err := c.repoArgs(repo)
	if err != nil {
		return nil, err
	}
	args := []string{"pr", "edit", strconv.Itoa(prID)}
	args = append(args, selectors...)
	args = append(args, "--title", title, "--body", body, "--json")
	var envelope bktPRViewEnvelope
	if err := c.runJSON(ctx, "PR update", &envelope, args...); err != nil {
		return nil, err
	}
	if err := validateRepoEnvelope(repo, envelope.Workspace, envelope.Repo); err != nil {
		return nil, err
	}
	if envelope.PullRequest.ID != prID {
		return nil, fmt.Errorf("bkt PR update returned pull request %d, want %d", envelope.PullRequest.ID, prID)
	}
	// Re-read the exact PR after editing so a successful command cannot
	// fabricate a completed update.
	return c.GetPR(ctx, repo, prID)
}

func (c *BKTClient) GetPR(ctx context.Context, repo RepoRef, prID int) (*PullRequest, error) {
	selectors, err := c.repoArgs(repo)
	if err != nil {
		return nil, err
	}
	args := []string{"pr", "view", strconv.Itoa(prID)}
	args = append(args, selectors...)
	args = append(args, "--json")
	var envelope bktPRViewEnvelope
	if err := c.runJSON(ctx, "PR read", &envelope, args...); err != nil {
		return nil, err
	}
	if err := validateRepoEnvelope(repo, envelope.Workspace, envelope.Repo); err != nil {
		return nil, err
	}
	if envelope.PullRequest.ID != prID {
		return nil, fmt.Errorf("bkt PR read returned pull request %d, want %d", envelope.PullRequest.ID, prID)
	}
	return bktPRToPullRequest(repo, envelope.PullRequest)
}

func validateRepoEnvelope(repo RepoRef, workspace, repoSlug string) error {
	if !strings.EqualFold(strings.TrimSpace(workspace), strings.TrimSpace(repo.Workspace)) ||
		!strings.EqualFold(strings.TrimSpace(repoSlug), strings.TrimSpace(repo.RepoSlug)) {
		return fmt.Errorf("bkt returned a mismatched repository (wanted %s/%s)", repo.Workspace, repo.RepoSlug)
	}
	return nil
}

func bktPRToPullRequest(repo RepoRef, pr bktPullRequest) (*PullRequest, error) {
	if pr.ID <= 0 {
		return nil, errors.New("bkt returned a pull request without an ID")
	}
	rawURL := strings.TrimSpace(pr.Links.HTML.Href)
	if rawURL != "" {
		parsed, err := url.Parse(rawURL)
		if err != nil || !strings.EqualFold(parsed.Hostname(), "bitbucket.org") {
			return nil, errors.New("bkt returned a pull request URL for an unexpected host")
		}
		wantPath := fmt.Sprintf("/%s/%s/pull-requests/%d", repo.Workspace, repo.RepoSlug, pr.ID)
		if !strings.EqualFold(strings.TrimRight(parsed.Path, "/"), wantPath) {
			return nil, errors.New("bkt returned a pull request URL for a mismatched repository")
		}
	}
	return &PullRequest{
		ID:               pr.ID,
		URL:              prURL(repo, pr.ID, rawURL),
		State:            strings.TrimSpace(pr.State),
		SourceCommitHash: strings.TrimSpace(pr.Source.Commit.Hash),
	}, nil
}

type bktChecksEnvelope struct {
	Workspace   string         `json:"workspace"`
	Repo        string         `json:"repo"`
	PullRequest int            `json:"pull_request"`
	Commit      string         `json:"commit"`
	Statuses    []CommitStatus `json:"statuses"`
}

func (c *BKTClient) ListPRStatuses(ctx context.Context, repo RepoRef, prID int) ([]CommitStatus, error) {
	selectors, err := c.repoArgs(repo)
	if err != nil {
		return nil, err
	}
	args := []string{"pr", "checks", strconv.Itoa(prID)}
	args = append(args, selectors...)
	args = append(args, "--json")
	var envelope bktChecksEnvelope
	if err := c.runJSON(ctx, "PR checks", &envelope, args...); err != nil {
		return nil, err
	}
	if err := validateRepoEnvelope(repo, envelope.Workspace, envelope.Repo); err != nil {
		return nil, err
	}
	if envelope.PullRequest != prID {
		return nil, fmt.Errorf("bkt PR checks returned pull request %d, want %d", envelope.PullRequest, prID)
	}
	return envelope.Statuses, nil
}

type bktPipeline struct {
	UUID   string `json:"uuid"`
	Target struct {
		Ref struct {
			Name string `json:"name"`
		} `json:"ref"`
	} `json:"target"`
}

type bktPipelineStep struct {
	UUID  string `json:"uuid"`
	Name  string `json:"name"`
	State struct {
		Name   string `json:"name"`
		Result struct {
			Name string `json:"name"`
		} `json:"result"`
	} `json:"state"`
	Result struct {
		Name string `json:"name"`
	} `json:"result"`
}

type bktPipelineView struct {
	Pipeline bktPipeline       `json:"pipeline"`
	Steps    []bktPipelineStep `json:"steps"`
}

func (c *BKTClient) pipelineView(ctx context.Context, repo RepoRef, pipelineUUID string) (*bktPipelineView, error) {
	selectors, err := c.repoArgs(repo)
	if err != nil {
		return nil, err
	}
	args := []string{"pipeline", "view", pipelineUUID}
	args = append(args, selectors...)
	args = append(args, "--json")
	var view bktPipelineView
	if err := c.runJSON(ctx, "pipeline read", &view, args...); err != nil {
		return nil, err
	}
	if normalizePipelineUUID(view.Pipeline.UUID) != normalizePipelineUUID(pipelineUUID) {
		return nil, errors.New("bkt pipeline read returned a mismatched pipeline")
	}
	return &view, nil
}

func (c *BKTClient) GetStepLog(ctx context.Context, repo RepoRef, pipelineUUID, stepUUID string) (string, error) {
	selectors, err := c.repoArgs(repo)
	if err != nil {
		return "", err
	}
	args := []string{"pipeline", "logs", pipelineUUID}
	args = append(args, selectors...)
	// bkt 0.30.0 accepts the inherited --json flag for this command while the
	// log payload itself remains text. Capture only its bounded tail.
	args = append(args, "--step", stepUUID, "--json")
	output, err := c.runTail(ctx, bktCommandTimeout, "pipeline logs", maxStepLogBytes, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// FetchFailedCheckLogs uses the exact immutable pipeline UUID exposed by the
// failing PR check. It deliberately declines a branch-only pipeline-list
// fallback because bkt 0.30.0's structured list schema omits the commit SHA.
func (c *BKTClient) FetchFailedCheckLogs(ctx context.Context, repo RepoRef, prID int, _ string, _ string, failingNames []string) (string, error) {
	statuses, err := c.ListPRStatuses(ctx, repo, prID)
	if err != nil {
		return "", err
	}
	targets := failedPipelineUUIDs(statuses, failingNames)
	if len(targets) == 0 {
		return "", nil
	}
	ordered := make([]string, 0, len(targets))
	for target := range targets {
		ordered = append(ordered, target)
	}
	sort.Strings(ordered)
	for _, target := range ordered {
		view, err := c.pipelineView(ctx, repo, target)
		if err != nil {
			continue
		}
		for _, step := range view.Steps {
			result := strings.TrimSpace(step.State.Result.Name)
			if result == "" {
				result = strings.TrimSpace(step.Result.Name)
			}
			if !strings.EqualFold(result, "FAILED") {
				continue
			}
			logs, err := c.GetStepLog(ctx, repo, view.Pipeline.UUID, step.UUID)
			if err == nil && strings.TrimSpace(logs) != "" {
				return strings.TrimSpace(logs), nil
			}
		}
	}
	return "", nil
}

func (c *BKTClient) runJSON(ctx context.Context, label string, destination any, args ...string) error {
	return c.runJSONWithTimeout(ctx, bktCommandTimeout, label, destination, args...)
}

func (c *BKTClient) runJSONWithTimeout(ctx context.Context, timeout time.Duration, label string, destination any, args ...string) error {
	output, err := c.runPrefix(ctx, timeout, label, maxBKTJSONBytes, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(output, destination); err != nil {
		return fmt.Errorf("bkt %s: decode JSON: %w", label, err)
	}
	return nil
}

func (c *BKTClient) runPrefix(ctx context.Context, timeout time.Duration, label string, limit int, args ...string) ([]byte, error) {
	capture := &prefixCapture{limit: limit}
	if err := c.run(ctx, timeout, label, capture, args...); err != nil {
		return nil, err
	}
	if capture.overflow {
		return nil, fmt.Errorf("bkt %s output exceeded %d bytes", label, limit)
	}
	return append([]byte(nil), capture.data...), nil
}

func (c *BKTClient) runTail(ctx context.Context, timeout time.Duration, label string, limit int, args ...string) ([]byte, error) {
	capture := &tailCapture{limit: limit}
	if err := c.run(ctx, timeout, label, capture, args...); err != nil {
		return nil, err
	}
	return append([]byte(nil), capture.data...), nil
}

func (c *BKTClient) run(ctx context.Context, timeout time.Duration, label string, stdout interface{ Write([]byte) (int, error) }, args ...string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("bkt %s: %w", label, err)
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := c.factory(commandCtx, "bkt", args...)
	if cmd == nil {
		return fmt.Errorf("bkt %s failed: executable is unavailable", label)
	}
	shellenv.ConfigureShellCommand(cmd)
	stderr := &prefixCapture{limit: maxBKTStderrBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := shellenv.RunShellCommand(cmd)
	if commandErr := commandCtx.Err(); commandErr != nil {
		return fmt.Errorf("bkt %s: %w", label, commandErr)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if detail := sanitizeBKTStderr(stderr); detail != "" {
				return fmt.Errorf("bkt %s failed (exit code %d): %s", label, exitErr.ExitCode(), detail)
			}
			return fmt.Errorf("bkt %s failed (exit code %d)", label, exitErr.ExitCode())
		}
		return fmt.Errorf("bkt %s failed: executable is unavailable", label)
	}
	return nil
}

var (
	bktCredentialAssignmentPattern = regexp.MustCompile(`(?i)([\w-]*(?:token|password|passwd|secret|credential|api[_-]?key)[\w-]*\s*[:=]\s*)(\S+)`)
	bktAuthSchemePattern           = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9+/=_.-]{8,}`)
	bktSecretShapedTokenPattern    = regexp.MustCompile(`\b(?:AT(?:BB|ATT|CTT)|BBDC-)[A-Za-z0-9+/=_.-]{4,}`)
)

// sanitizeBKTStderr returns the bounded stderr capture with URL userinfo,
// credential-style assignments, auth-scheme values, and Atlassian-shaped
// tokens redacted, so bkt diagnostics can reach step logs without ever
// carrying a secret.
func sanitizeBKTStderr(capture *prefixCapture) string {
	text := strings.ToValidUTF8(strings.TrimSpace(string(capture.data)), "")
	if text == "" {
		return ""
	}
	text = safeurl.RedactText(text)
	text = bktAuthSchemePattern.ReplaceAllString(text, "$1 [redacted]")
	text = bktCredentialAssignmentPattern.ReplaceAllString(text, "${1}[redacted]")
	text = bktSecretShapedTokenPattern.ReplaceAllString(text, "[redacted]")
	if capture.overflow {
		text += " [stderr truncated]"
	}
	return text
}

type prefixCapture struct {
	limit    int
	data     []byte
	overflow bool
}

func (c *prefixCapture) Write(p []byte) (int, error) {
	if c.limit <= 0 {
		c.overflow = c.overflow || len(p) > 0
		return len(p), nil
	}
	remaining := c.limit - len(c.data)
	if remaining > 0 {
		keep := min(remaining, len(p))
		c.data = append(c.data, p[:keep]...)
	}
	if len(p) > remaining {
		c.overflow = true
	}
	return len(p), nil
}

type tailCapture struct {
	limit int
	data  []byte
}

func (c *tailCapture) Write(p []byte) (int, error) {
	if c.limit <= 0 {
		return len(p), nil
	}
	if len(p) >= c.limit {
		c.data = append(c.data[:0], p[len(p)-c.limit:]...)
		return len(p), nil
	}
	overflow := len(c.data) + len(p) - c.limit
	if overflow > 0 {
		copy(c.data, c.data[overflow:])
		c.data = c.data[:len(c.data)-overflow]
	}
	c.data = append(c.data, p...)
	return len(p), nil
}
