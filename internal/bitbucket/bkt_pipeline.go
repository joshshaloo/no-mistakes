package bitbucket

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func normalizePipelineUUID(raw string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(raw), "{}"))
}

func pipelineUUIDFromStatusURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	for _, fragment := range []string{parsed.Fragment, parsed.Path} {
		idx := strings.LastIndex(fragment, "/results/")
		if idx < 0 {
			continue
		}
		id := strings.SplitN(fragment[idx+len("/results/"):], "?", 2)[0]
		return normalizePipelineUUID(strings.SplitN(id, "/", 2)[0])
	}
	return ""
}

func failedPipelineUUIDs(statuses []CommitStatus, names []string) map[string]struct{} {
	selected := map[string]bool{}
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			selected[name] = true
		}
	}
	if len(selected) == 0 {
		return nil
	}
	ids := map[string]struct{}{}
	for _, status := range LatestStatuses(statuses) {
		if !selected[statusName(status)] {
			continue
		}
		if id := pipelineUUIDFromStatusURL(status.URL); id != "" {
			ids[id] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// FetchFailedCheckTargetLogs preserves upstream's per-target identity without
// using bkt's branch-only pipeline list, which cannot prove a commit binding.
func (c *BKTClient) FetchFailedCheckTargetLogs(ctx context.Context, repo RepoRef, prID int, _, _ string, selected []scm.CheckTarget) ([]scm.FailedCheckLog, error) {
	statuses, err := c.ListPRStatuses(ctx, repo, prID)
	if err != nil {
		return nil, err
	}
	statuses = LatestStatuses(statuses)
	results := make([]scm.FailedCheckLog, 0, len(selected))
	for _, target := range selected {
		ids := map[string]struct{}{}
		for _, status := range statuses {
			if target.ProviderID != "" {
				if statusProviderID(status) != target.ProviderID {
					continue
				}
			} else if statusName(status) != target.Name {
				continue
			}
			if statusBucket(status.State) != scm.CheckBucketFail {
				continue
			}
			if id := pipelineUUIDFromStatusURL(status.URL); id != "" {
				ids[id] = struct{}{}
			}
		}
		result := scm.FailedCheckLog{Target: target}
		if len(ids) == 0 {
			result.Err = fmt.Errorf("selected Bitbucket check %q has no failing pipeline identity", target.Identity())
			results = append(results, result)
			continue
		}
		ordered := make([]string, 0, len(ids))
		for id := range ids {
			ordered = append(ordered, id)
		}
		sort.Strings(ordered)
		var logs []string
		var failures []error
		for _, id := range ordered {
			view, err := c.pipelineView(ctx, repo, id)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			for _, step := range view.Steps {
				state := strings.TrimSpace(step.State.Result.Name)
				if state == "" {
					state = strings.TrimSpace(step.Result.Name)
				}
				if !strings.EqualFold(state, "FAILED") {
					continue
				}
				text, err := c.GetStepLog(ctx, repo, view.Pipeline.UUID, step.UUID)
				if err != nil {
					failures = append(failures, err)
					continue
				}
				if text != "" {
					logs = append(logs, text)
				}
			}
		}
		result.Output = strings.Join(logs, "\n\n")
		if len(result.Output) > maxStepLogBytes {
			result.Output = result.Output[len(result.Output)-maxStepLogBytes:]
		}
		result.Err = errors.Join(failures...)
		results = append(results, result)
	}
	return results, nil
}
