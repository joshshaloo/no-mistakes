package main

import "testing"

func TestFakeGitHubModeDoesNotDependOnExecutableName(t *testing.T) {
	for _, mode := range []string{"gh", "ci-gh", "ci-gh-seq", "ci-gh-reconcile", "ci-gh-with-intervening-push"} {
		if !fakeGitHubMode(mode) {
			t.Errorf("fakeGitHubMode(%q) = false", mode)
		}
	}
	for _, mode := range []string{"glab", "ci-glab", "git-passthrough", ""} {
		if fakeGitHubMode(mode) {
			t.Errorf("fakeGitHubMode(%q) = true", mode)
		}
	}
}
