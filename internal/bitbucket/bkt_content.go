package bitbucket

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GetPRContent reads the explicit raw body rather than inferring empty content
// from a missing field when upstream preserves author-owned PR descriptions.
func (c *BKTClient) GetPRContent(ctx context.Context, repo RepoRef, id int) (scm.PRContent, error) {
	selectors, err := c.repoArgs(repo)
	if err != nil {
		return scm.PRContent{}, err
	}
	args := append([]string{"pr", "view", strconv.Itoa(id)}, selectors...)
	args = append(args, "--json")
	var envelope struct {
		Workspace string `json:"workspace"`
		Repo      string `json:"repo"`
		PR        struct {
			ID          int     `json:"id"`
			Title       string  `json:"title"`
			Description *string `json:"description"`
			Summary     *struct {
				Raw *string `json:"raw"`
			} `json:"summary"`
		} `json:"pull_request"`
	}
	if err := c.runJSON(ctx, "PR content", &envelope, args...); err != nil {
		return scm.PRContent{}, err
	}
	if err := validateRepoEnvelope(repo, envelope.Workspace, envelope.Repo); err != nil {
		return scm.PRContent{}, err
	}
	body := envelope.PR.Description
	if envelope.PR.Summary != nil && envelope.PR.Summary.Raw != nil {
		body = envelope.PR.Summary.Raw
	}
	if envelope.PR.ID != id || strings.TrimSpace(envelope.PR.Title) == "" || body == nil {
		return scm.PRContent{}, fmt.Errorf("bkt PR content: incomplete or mismatched raw content")
	}
	return scm.PRContent{Title: envelope.PR.Title, Body: *body}, nil
}
