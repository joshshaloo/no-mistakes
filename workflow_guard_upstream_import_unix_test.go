//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"gopkg.in/yaml.v3"
)

// Execute the actual Ubuntu workflow script against real, isolated Git repos.
// The canonical upstream URL is redirected locally by Git configuration, not by
// a production override that could turn an arbitrary repo into the authority.
func TestGuardGeneratedFilesUpstreamImport(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("the Ubuntu workflow fixture needs bash")
	}
	data, err := os.ReadFile(".github/workflows/guard-generated-files.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow wfDoc
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["check"].Steps {
		if strings.Contains(step.Run, "git diff") {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("generated-files guard script missing")
	}

	const tag = "v9.8.7"
	upstream := t.TempDir()
	guardGit(t, upstream, "init", "-q")
	guardWrite(t, upstream, "CHANGELOG.md", "upstream release notes\n")
	guardWrite(t, upstream, ".release-please-manifest.json", "{\".\":\"9.8.7\"}\n")
	guardGit(t, upstream, "add", ".")
	guardGit(t, upstream, "commit", "-qm", "release")
	guardGit(t, upstream, "tag", "-a", tag, "-m", "upstream release")

	for _, tc := range []struct {
		name   string
		change string
		wantOK bool
	}{
		{"unchanged_does_not_need_network", "unchanged", true},
		{"complete_import_ignores_poisoned_local_tag", "exact", true},
		{"manual_changelog_edit", "changelog", false},
		{"manifest_whitespace_is_not_byte_identical", "manifest", false},
		{"unchanged_other_file_must_also_match_upstream", "partial", false},
		{"deleted_generated_file", "delete", false},
		{"renamed_generated_file", "rename", false},
		{"generated_file_mode_changed", "mode", false},
		{"missing_pin", "missing-pin", false},
		{"malformed_pin", "bad-pin", false},
		{"missing_remote_tag", "missing-tag", false},
		{"fetch_failure_cannot_use_cached_authority", "offline", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			guardGit(t, repo, "init", "-q")
			guardWrite(t, repo, "CHANGELOG.md", "fork release notes\n")
			guardWrite(t, repo, ".release-please-manifest.json", "{\".\":\"0.0.1\"}\n")
			guardGit(t, repo, "add", ".")
			guardGit(t, repo, "commit", "-qm", "base")
			base := guardGit(t, repo, "rev-parse", "HEAD")
			// A same-named tag from the fork must never stand in for upstream.
			guardGit(t, repo, "tag", tag)
			guardWrite(t, repo, ".github/upstream-release-tag", tag+"\n")
			if tc.change != "unchanged" && tc.change != "rename" {
				guardWrite(t, repo, "CHANGELOG.md", "upstream release notes\n")
				if tc.change != "partial" {
					guardWrite(t, repo, ".release-please-manifest.json", "{\".\":\"9.8.7\"}\n")
				}
			}
			switch tc.change {
			case "changelog":
				guardWrite(t, repo, "CHANGELOG.md", "hand-edited release notes\n")
			case "manifest":
				guardWrite(t, repo, ".release-please-manifest.json", "{ \".\": \"9.8.7\" }\n")
			case "delete":
				if err := os.Remove(filepath.Join(repo, "CHANGELOG.md")); err != nil {
					t.Fatal(err)
				}
			case "rename":
				guardGit(t, repo, "mv", "CHANGELOG.md", "renamed-notes.md")
			case "missing-pin":
				if err := os.Remove(filepath.Join(repo, ".github", "upstream-release-tag")); err != nil {
					t.Fatal(err)
				}
			case "bad-pin":
				guardWrite(t, repo, ".github/upstream-release-tag", "v9.8.7; touch injected\n")
			case "missing-tag":
				guardWrite(t, repo, ".github/upstream-release-tag", "v9.8.8\n")
			}
			guardGit(t, repo, "add", "-A")
			if tc.change == "mode" {
				guardGit(t, repo, "update-index", "--chmod=+x", "CHANGELOG.md")
			}
			guardGit(t, repo, "commit", "-qm", "candidate")
			head := guardGit(t, repo, "rev-parse", "HEAD")
			remote := upstream
			if tc.change == "offline" || tc.change == "unchanged" {
				remote = filepath.Join(t.TempDir(), "absent-upstream")
				guardGit(t, repo, "update-ref", "refs/no-mistakes/generated-release", head)
			}
			guardGit(t, repo, "config", "url."+remote+".insteadOf", "https://github.com/kunchenguid/no-mistakes.git")
			// Checkout/working-tree bytes are not the event's immutable head.
			guardWrite(t, repo, ".github/upstream-release-tag", "invalid-checkout-pin\n")
			guardWrite(t, repo, "CHANGELOG.md", "unrelated checkout content\n")
			out, err := guardCommand(t, repo, []string{"BASE_SHA=" + base, "HEAD_SHA=" + head}, bash, "-c", script)
			if (err == nil) != tc.wantOK {
				t.Fatalf("guard error=%v, want success=%v:\n%s", err, tc.wantOK, out)
			}
			if _, err := os.Stat(filepath.Join(repo, "injected")); !os.IsNotExist(err) {
				t.Fatalf("pin must never execute as shell source: %v", err)
			}
		})
	}
}

func guardWrite(t *testing.T, dir, name, contents string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func guardGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := guardCommand(t, dir, nil, "git", args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func guardCommand(t *testing.T, dir string, extraEnv []string, program string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") && key != "BASE_SHA" && key != "HEAD_SHA" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Guard test", "GIT_AUTHOR_EMAIL=guard@example.invalid",
		"GIT_COMMITTER_NAME=Guard test", "GIT_COMMITTER_EMAIL=guard@example.invalid",
		"GIT_ALLOW_PROTOCOL=file")
	cmd.Env = append(cmd.Env, extraEnv...)
	shellenv.ConfigureShellCommand(cmd)
	return shellenv.CombinedOutputShellCommand(cmd)
}
