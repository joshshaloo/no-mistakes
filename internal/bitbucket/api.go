package bitbucket

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// API is the transport-neutral subset of Bitbucket Cloud operations used by
// No Mistakes. Client implements it with direct REST credentials; BKTClient
// implements it by invoking the authenticated bkt CLI.
type API interface {
	Available(context.Context) error
	FindOpenPRBySourceBranch(context.Context, RepoRef, string, string) (*PullRequest, error)
	CreatePR(context.Context, RepoRef, string, string, string, string, bool) (*PullRequest, error)
	UpdatePR(context.Context, RepoRef, int, string, string) (*PullRequest, error)
	AddPRComment(context.Context, RepoRef, int, string) error
	ListPRComments(context.Context, RepoRef, int) ([]string, error)
	GetPR(context.Context, RepoRef, int) (*PullRequest, error)
	ListPRStatuses(context.Context, RepoRef, int) ([]CommitStatus, error)
}

// pipelineAPI is the REST transport's commit-bound pipeline discovery. bkt
// instead targets immutable UUIDs from PR checks; it must never list by branch.
type pipelineAPI interface {
	ListPipelinesByCommit(context.Context, RepoRef, string) ([]Pipeline, error)
	ListPipelineSteps(context.Context, RepoRef, string) ([]PipelineStep, error)
	GetStepLog(context.Context, RepoRef, string, string) (string, error)
}

// CommandFactory creates a direct (non-shell) command for a caller-controlled
// context. Pipeline callers use it to apply the step worktree and environment.
type CommandFactory func(context.Context, string, ...string) *exec.Cmd

// NewAPIFromEnvOrBKT preserves the public direct-REST credential path when both
// variables are present. bkt is considered only when both are absent; a partial
// direct configuration remains an explicit error rather than being hidden by a
// fallback transport.
func NewAPIFromEnvOrBKT(env []string, factory CommandFactory) (API, error) {
	email := strings.TrimSpace(lookupEnv(env, envEmail))
	token := strings.TrimSpace(lookupEnv(env, envToken))
	switch {
	case email != "" && token != "":
		return NewClientFromEnv(env)
	case email != "":
		return nil, fmt.Errorf("missing %s", envToken)
	case token != "":
		return nil, fmt.Errorf("missing %s", envEmail)
	default:
		return NewBKTClient(factory), nil
	}
}

// NewAPIFromEnvironment consumes a fully resolved environment (nil inherits
// the process). Explicitly absent keys must not resurrect ambient credentials
// that a repository forge profile removed.
func NewAPIFromEnvironment(env []string, factory CommandFactory) (API, error) {
	if env == nil {
		env = os.Environ()
	}
	resolved := append([]string{}, env...)
	for _, key := range []string{envEmail, envToken, envAPIBaseURL} {
		present := false
		for _, entry := range resolved {
			if strings.HasPrefix(entry, key+"=") {
				present = true
				break
			}
		}
		if !present {
			resolved = append(resolved, key+"=")
		}
	}
	return NewAPIFromEnvOrBKT(resolved, factory)
}

func (c *Client) Available(_ context.Context) error {
	if c == nil {
		return fmt.Errorf("bitbucket REST client is not configured")
	}
	return nil
}
