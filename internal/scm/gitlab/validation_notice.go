package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/notices"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) PublishPRNotice(ctx context.Context, pr *scm.PR, body string) error {
	id := pr.Number
	if id == "" {
		var err error
		id, err = scm.ExtractPRNumber(pr.URL)
		if err != nil {
			return err
		}
	}
	cmd := h.cmd(ctx, "glab", "mr", "note", id, "--message", body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("glab mr note: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (h *Host) ListPRNotices(ctx context.Context, pr *scm.PR) ([]string, error) {
	if h.projectPath == "" {
		return nil, errors.New("glab API MR notes: missing project path")
	}
	id := pr.Number
	if id == "" {
		var err error
		id, err = scm.ExtractPRNumber(pr.URL)
		if err != nil {
			return nil, err
		}
	}
	segments := strings.Split(h.projectPath, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	base := fmt.Sprintf("projects/%s/merge_requests/%s/notes?order_by=created_at&sort=asc&per_page=100", strings.Join(segments, "%2F"), id)
	var bodies []string
	totalBytes := 0
	for page := 1; ; page++ {
		if err := notices.ValidateAggregate(page-1, len(bodies), totalBytes, true); err != nil {
			return nil, fmt.Errorf("read GitLab MR notices: %w", err)
		}
		endpoint := fmt.Sprintf("%s&page=%d", base, page)
		out, err := notices.RunCommandLimit(h.cmd(ctx, "glab", "api", "--include", endpoint).Cmd, "read GitLab MR notices", notices.MaxResponseBytes-totalBytes)
		if err != nil {
			return nil, err
		}
		totalBytes += len(out)
		headers, payload, err := notices.SplitIncludedResponse(out)
		if err != nil {
			return nil, fmt.Errorf("parse glab MR notes response: %w", err)
		}
		var notes []struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(payload, &notes); err != nil {
			return nil, fmt.Errorf("parse glab MR notes: %w", err)
		}
		hasNext := strings.TrimSpace(headers["x-next-page"]) != ""
		if err := notices.ValidateAggregate(page, len(bodies)+len(notes), totalBytes, hasNext); err != nil {
			return nil, fmt.Errorf("read GitLab MR notices: %w", err)
		}
		for _, note := range notes {
			bodies = append(bodies, note.Body)
		}
		if !hasNext {
			return bodies, nil
		}
	}
}
