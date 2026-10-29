package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Explicitly opted-in fixtures supply the two REST proofs used by stale-risk
// readiness. Do not turn arbitrary Actions endpoints into successful evidence:
// the repository, job, run, attempt, check name and head must all agree.
func fakeActionsAttemptCommands(args []string) {
	head := os.Getenv("FAKE_CLI_ACTIONS_HEAD")
	if head == "" || strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") != "gh" || len(args) != 2 || args[0] != "api" {
		return
	}
	attempt := 1
	if value := os.Getenv("FAKE_CLI_ACTIONS_ATTEMPT"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			fmt.Fprintln(os.Stderr, "invalid fake Actions attempt")
			os.Exit(1)
		}
		attempt = parsed
	}
	job := 456
	if attempt == 2 {
		job = 789
	}
	var response any
	switch args[1] {
	case fmt.Sprintf("repos/test/repo/actions/jobs/%d", job):
		response = map[string]any{"id": job, "run_id": 123, "run_attempt": attempt, "head_sha": head, "name": "test"}
	case fmt.Sprintf("repos/test/repo/actions/runs/123/attempts/%d", attempt):
		response = map[string]any{"id": 123, "run_attempt": attempt, "head_sha": head, "repository": map[string]string{"full_name": "test/repo"}}
	default:
		return
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
