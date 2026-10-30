package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/notices"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) ValidateValidationNoticeSupport() error {
	host := strings.ToLower(strings.TrimSpace(h.host))
	if host != "github.com" {
		if host == "" {
			host = "unknown"
		}
		return fmt.Errorf("GitHub validation notices are verified only for github.com; host %q is unsupported because no real GitHub Enterprise host was available for verification", host)
	}
	return nil
}

func (h *Host) PublishPRNotice(ctx context.Context, pr *scm.PR, body string) error {
	if err := h.ValidateValidationNoticeSupport(); err != nil {
		return err
	}
	selector, err := prSelector(pr)
	if err != nil {
		return err
	}
	args := append([]string{"pr", "comment", selector}, h.repoArgs()...)
	args = append(args, "--body-file", "-")
	cmd := h.cmd(ctx, "gh", args...)
	cmd.Stdin = strings.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr comment: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (h *Host) ListPRNotices(ctx context.Context, pr *scm.PR) ([]string, error) {
	if err := h.ValidateValidationNoticeSupport(); err != nil {
		return nil, err
	}
	if h.repo == "" {
		return nil, errors.New("gh api PR comments: missing repository")
	}
	selector, err := prSelector(pr)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("repos/%s/issues/%s/comments", h.repo, selector)
	var bodies []string
	totalBytes := 0
	for page := 1; ; page++ {
		if err := notices.ValidateAggregate(page-1, len(bodies), totalBytes, true); err != nil {
			return nil, fmt.Errorf("read GitHub PR notices: %w", err)
		}
		cmd := h.cmd(ctx, "gh", "api", "--include", "--method", "GET", endpoint, "-f", "per_page=100", "-f", fmt.Sprintf("page=%d", page))
		out, err := notices.RunCommandLimit(cmd.Cmd, "read GitHub PR notices", notices.MaxResponseBytes-totalBytes)
		if err != nil {
			return nil, err
		}
		totalBytes += len(out)
		headers, payload, err := notices.SplitIncludedResponse(out)
		if err != nil {
			return nil, fmt.Errorf("parse gh PR comments response: %w", err)
		}
		var comments []struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(payload, &comments); err != nil {
			return nil, fmt.Errorf("parse gh PR comments: %w", err)
		}
		hasNext := strings.Contains(headers["link"], `rel="next"`)
		if err := notices.ValidateAggregate(page, len(bodies)+len(comments), totalBytes, hasNext); err != nil {
			return nil, fmt.Errorf("read GitHub PR notices: %w", err)
		}
		for _, comment := range comments {
			bodies = append(bodies, comment.Body)
		}
		if !hasNext {
			return bodies, nil
		}
	}
}

// GetCIAttemptIdentity proves provider run-attempt identity for every check;
// a details URL by itself is never evidence of a fresh CI attempt.
func (h *Host) GetCIAttemptIdentity(ctx context.Context, _ *scm.PR, headSHA string, checks []scm.Check) (string, error) {
	if err := h.ValidateValidationNoticeSupport(); err != nil {
		return "", err
	}
	repo := h.apiRepoSlug()
	if repo == "" {
		return "", errors.New("GitHub repository is unknown")
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return "", errors.New("exact PR head is unknown")
	}
	var identities []string
	for _, check := range checks {
		locator := check.DetailsURL
		if locator == "" {
			locator = check.Link
		}
		runID, jobID, err := h.actionsJobLocator(locator, repo)
		if err != nil {
			return "", fmt.Errorf("check %q is not an authoritative GitHub Actions job: %w", check.Name, err)
		}
		var job struct {
			ID         int64  `json:"id"`
			RunID      int64  `json:"run_id"`
			RunAttempt int    `json:"run_attempt"`
			HeadSHA    string `json:"head_sha"`
			Name       string `json:"name"`
		}
		if err := h.getAPIJSON(ctx, fmt.Sprintf("repos/%s/actions/jobs/%d", repo, jobID), &job); err != nil {
			return "", fmt.Errorf("read GitHub Actions job %d for check %q: %w", jobID, check.Name, err)
		}
		if job.ID != jobID || job.RunID != runID || job.RunAttempt < 1 || !strings.EqualFold(strings.TrimSpace(job.HeadSHA), headSHA) || strings.TrimSpace(job.Name) != strings.TrimSpace(check.Name) {
			return "", fmt.Errorf("GitHub Actions job %d does not establish the exact head and run attempt for check %q", jobID, check.Name)
		}
		var attempt struct {
			ID         int64  `json:"id"`
			RunAttempt int    `json:"run_attempt"`
			HeadSHA    string `json:"head_sha"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		endpoint := fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d", repo, job.RunID, job.RunAttempt)
		if err := h.getAPIJSON(ctx, endpoint, &attempt); err != nil {
			return "", fmt.Errorf("read GitHub Actions run %d attempt %d for check %q: %w", job.RunID, job.RunAttempt, check.Name, err)
		}
		if attempt.ID != job.RunID || attempt.RunAttempt != job.RunAttempt || !strings.EqualFold(strings.TrimSpace(attempt.HeadSHA), headSHA) || !strings.EqualFold(strings.TrimSpace(attempt.Repository.FullName), repo) {
			return "", fmt.Errorf("GitHub Actions run %d attempt %d is not bound to repository %s and head %s", job.RunID, job.RunAttempt, repo, headSHA)
		}
		identities = append(identities, fmt.Sprintf("github:%s:run:%d:attempt:%d", strings.ToLower(repo), job.RunID, job.RunAttempt))
	}
	if len(identities) == 0 {
		return "", errors.New("GitHub reported no checks with authoritative Actions attempt identity")
	}
	sort.Strings(identities)
	return strings.Join(identities, "\x00"), nil
}

func (h *Host) apiRepoSlug() string {
	repo := strings.Trim(strings.TrimSpace(h.repo), "/")
	host := strings.Trim(strings.TrimSpace(h.host), "/")
	if host != "" && !strings.EqualFold(host, "github.com") {
		repo = strings.TrimPrefix(repo, host+"/")
	}
	if strings.Count(repo, "/") != 1 {
		return ""
	}
	return repo
}

func (h *Host) actionsJobLocator(rawURL, repo string) (int64, int64, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return 0, 0, errors.New("details URL is invalid")
	}
	expectedHost := strings.TrimSpace(h.host)
	if expectedHost == "" {
		expectedHost = "github.com"
	}
	if !strings.EqualFold(u.Hostname(), expectedHost) {
		return 0, 0, fmt.Errorf("details host %q does not match %q", u.Hostname(), expectedHost)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	repoParts := strings.Split(repo, "/")
	if len(parts) != 7 || len(repoParts) != 2 || !strings.EqualFold(parts[0], repoParts[0]) || !strings.EqualFold(parts[1], repoParts[1]) || parts[2] != "actions" || parts[3] != "runs" || parts[5] != "job" {
		return 0, 0, errors.New("details URL is not an Actions job URL for this repository")
	}
	runID, runErr := strconv.ParseInt(parts[4], 10, 64)
	jobID, jobErr := strconv.ParseInt(parts[6], 10, 64)
	if runErr != nil || jobErr != nil || runID < 1 || jobID < 1 {
		return 0, 0, errors.New("Actions run or job ID is invalid")
	}
	return runID, jobID, nil
}

func (h *Host) getAPIJSON(ctx context.Context, endpoint string, dst any) error {
	out, err := notices.RunCommand(h.cmd(ctx, "gh", "api", endpoint).Cmd, "read GitHub Actions attempt")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, dst); err != nil {
		return fmt.Errorf("decode GitHub Actions attempt: %w", err)
	}
	return nil
}
