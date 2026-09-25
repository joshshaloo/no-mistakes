---
title: Provider Integration
description: Set up GitHub, GitLab, Bitbucket Cloud, or Azure DevOps for PR creation and CI monitoring.
---

The PR and CI steps need to talk to your git host. Four hosts are supported:
GitHub, GitLab, Bitbucket Cloud (`bitbucket.org`), and Azure DevOps
(`dev.azure.com` and legacy `*.visualstudio.com`). Everything else
short-circuits the PR and CI steps with `skipped`.

Provider integration is optional for the local gate. You only need it for the
steps that happen after validation: opening or updating the PR, watching hosted
CI, and fixing remote-only failures.

Without any provider setup, `no-mistakes` still gives you the local gate:

- rebase
- review
- test
- document
- lint
- push through normal Git transport

What you do not get is PR automation and CI monitoring.

## What each step needs

| Step | GitHub | GitLab | Bitbucket Cloud | Azure DevOps |
|---|---|---|---|---|
| **PR** (create/reuse + notices) | `gh` CLI, authenticated | `glab` CLI, authenticated | authenticated `bkt` CLI or `NO_MISTAKES_BITBUCKET_EMAIL` + `NO_MISTAKES_BITBUCKET_API_TOKEN` | `az` CLI + `azure-devops` extension, authenticated |
| **CI** (polling, auto-fix) | `gh` CLI | `glab` CLI | same `bkt` or env credential path | `az` CLI |
| **Merge conflict auto-fix** | `gh` CLI | `glab` CLI | not supported | `az` CLI |
| **Mergeability polling** | `gh` CLI | `glab` CLI | not supported | `az` CLI |
| **Failed check log fetching** | `gh` CLI | `glab` CLI | supported | not yet |

## What changes when provider wiring is present

Once the host is wired up, `no-mistakes` can keep owning the branch after it
pushes to the configured target:

- create the PR automatically or reuse an existing PR while publishing status as conversation notices
- keep polling hosted CI until the PR is merged, closed, declined, or the configured `ci_timeout` idle window elapses
- fetch failing job logs for the CI auto-fix loop
- on GitHub, GitLab, and Azure DevOps, watch mergeability and fix merge conflicts when possible

## GitHub

Install the GitHub CLI and authenticate:

```sh
# macOS
brew install gh

# Linux
# see https://github.com/cli/cli/blob/trunk/docs/install_linux.md

gh auth login
```

Verify:

```sh
gh auth status
```

`no-mistakes doctor` also checks for `gh` availability.
For PR and workflow-run commands, no-mistakes passes the repository slug from the recorded upstream remote or PR URL to `gh`, so daemon-run commands do not depend on the daemon's current working directory.

**What you get:**

- PR creation and update on pushes
- CI check polling with exponential backoff (30s → 60s → 120s) until the PR is merged, closed, or the configured `ci_timeout` idle window elapses
- Failed job log fetching (`gh run view --log-failed`) for the CI auto-fix step
- PR mergeability polling, and agent-driven resolution when the provider reports an actual merge conflict

### GitHub fork contributions

Fork routing is available for GitHub when you need to push branches to your fork but open PRs against the parent repository.
Keep `origin` pointed at the parent repository, then initialize with your fork URL:

```sh
git remote set-url origin git@github.com:parent-owner/repo.git
no-mistakes init --fork-url git@github.com:your-user/repo.git
```

With this setup, the push and CI auto-fix push steps update the fork, while the PR and CI steps stay scoped to the parent repository.
The GitHub PR step opens PRs with a fork-qualified head such as `your-user:feature-branch`.
Re-running `no-mistakes init` later preserves the stored fork URL unless you pass a new `--fork-url`.

Fork routing currently requires both `origin` and `--fork-url` to be GitHub remotes with owner/repo paths.
GitLab and Bitbucket fork MR/PR routing are not implemented yet; if a legacy or manually edited repo record has `fork_url` set for those providers, PR creation skips instead of opening an unsafe self PR.

## GitLab

Install the GitLab CLI and authenticate:

```sh
# macOS
brew install glab

# Linux
# see https://gitlab.com/gitlab-org/cli

glab auth login
```

**What you get:**

- PR (merge request) creation and update
- CI pipeline status polling until the merge request is merged, closed, or the configured `ci_timeout` idle window elapses
- Failed job trace fetching (`glab ci trace`) for the CI auto-fix step
- Merge-conflict polling and auto-fix, same as GitHub

## Bitbucket Cloud

Bitbucket Cloud supports two authentication paths. The preferred macOS path is [`bkt`](https://github.com/avivsinai/bitbucket-cli) **v0.30.0 or newer**, authenticated to Bitbucket Cloud through macOS Keychain:

```sh
bkt --version
bkt auth login https://bitbucket.org --kind cloud --web-token
bkt context create work --host bitbucket.org --workspace your-workspace --set-active
bkt auth status
no-mistakes doctor
```

Create a scoped Bitbucket application token with Account Read, Repository Read, Pull Request Read/Write, and Pipeline Read access so all PR and CI capabilities are available. General Atlassian tokens without the Bitbucket application scopes do not work.

`no-mistakes` invokes `bkt` directly with structured output and explicit workspace, repository, source, and target selectors parsed from the canonical `bitbucket.org` remote. It accepts only an authenticated Cloud context backed by `bkt`'s Keychain credential source; a Data Center or mismatched context cannot redirect provider operations. `bkt` remains the sole credential owner: No Mistakes never retrieves, exports, passes, or stores its token.

The existing direct REST path remains supported. Set both variables (and optionally the API base override):

```sh
export NO_MISTAKES_BITBUCKET_EMAIL=you@example.com
export NO_MISTAKES_BITBUCKET_API_TOKEN=your-api-token
export NO_MISTAKES_BITBUCKET_API_BASE_URL=https://api.bitbucket.org/2.0 # optional
```

When both direct credentials are present, they take precedence and preserve the existing REST behavior. `bkt` is selected only when both are absent. A partial direct configuration fails explicitly instead of silently falling back. Get a direct-path API token from [Atlassian API token settings](https://id.atlassian.com/manage-profile/security/api-tokens).

**What you get:**

- PR creation and update
- CI pipeline status polling until the PR is merged, declined, or the configured `ci_timeout` idle window elapses
- Failed pipeline step log fetching for the CI auto-fix step

**What you don't get (yet):**

- PR mergeability polling
- Merge-conflict auto-fix

These are GitHub, GitLab, and Azure DevOps only right now.

## Azure DevOps

Azure DevOps uses the Azure CLI with the `azure-devops` extension. Install both
and authenticate:

```sh
# macOS
brew install azure-cli

# Linux / Windows
# see https://learn.microsoft.com/en-us/cli/azure/install-azure-cli

az extension add --name azure-devops

# Authenticate with a Personal Access Token (Code: Read & Write, Pull Request
# Threads, Build: Read). Either run `az devops login` and paste the PAT, or
# export it for non-interactive use:
export AZURE_DEVOPS_EXT_PAT=your-pat
```

Create a PAT from **User settings → Personal access tokens** in your Azure
DevOps organization. The daemon inherits `AZURE_DEVOPS_EXT_PAT` from the
environment it runs under, the same way the GitHub backend inherits `gh` auth.

Both `https://dev.azure.com/{org}/{project}/_git/{repo}` and the legacy
`https://{org}.visualstudio.com/{project}/_git/{repo}` remotes are detected, as
well as their SSH forms (`git@ssh.dev.azure.com:v3/...`).

**What you get:**

- PR creation and update (`az repos pr create` / `update`); Azure DevOps caps
  PR descriptions at 4000 characters, so the pipeline builds the body within
  that budget - shedding the Testing section first when needed, then applying
  a final truncation backstop with a visible marker
- CI status polling - Azure branch policy evaluations (build validation and
  status checks) are read via `az repos pr policy list` until the PR is
  completed, abandoned, or the configured `ci_timeout` idle window elapses
- Merge-conflict polling and auto-fix from the PR's `mergeStatus`

**What you don't get (yet):**

- Failed check log fetching for the CI auto-fix step (the `az` CLI has no
  first-class build-log command)
- Fork PR routing (same as GitLab and Bitbucket)

## Self-hosted GitHub/GitLab

Self-hosted GitLab works through `glab`. GitHub Enterprise can still be detected through `gh`, but PR notice publication and related PR mutation are currently refused because that validation path has only been verified on `github.com`.

### Self-hosted GitHub Enterprise

GitHub Enterprise Server is detected when the host is one `gh` is authenticated against. For a non-`github.com` upstream, `no-mistakes` consults gh's configured hosts (`hosts.yml`, honoring `GH_CONFIG_DIR` then `XDG_CONFIG_HOME/gh`, then `~/.config/gh`). If the host is absent, detection fails closed and the upstream is treated as unsupported.

Detection does not currently authorize the PR path: no-mistakes refuses before publishing validation notices or making related PR mutations on GitHub Enterprise. The [CI step reference](/no-mistakes/reference/pipeline-steps/#ci) owns the verified-host restriction.

### Self-hosted GitLab

Self-hosted GitLab is detected out of the box even when the hostname carries no `gitlab` marker (for example `git.example.com`).
When the hostname is not obviously GitLab, `no-mistakes` consults glab's configured hosts (`config.yml`, honoring `GLAB_CONFIG_DIR` then `XDG_CONFIG_HOME/glab-cli`, then `~/.config/glab-cli`) and treats the upstream as GitLab if its host appears there as a configured host or `api_host`.
Running `glab auth login --hostname your-gitlab.example.com` is enough to make detection succeed; if glab is not configured for the host, detection fails closed and the upstream is treated as unsupported.

The GitLab backend is pinned against `glab v1.5x`. Self-hosted detection and the merge-request and CI steps rely on its current flag and API surface, so keep `glab` reasonably up to date.

## SSH host aliases

SSH remotes that use a host alias from your SSH configuration (for example `git@github-personal:owner/repo` or `git@gitlab-work:group/repo`, where `github-personal`/`gitlab-work` map to a real `HostName` via `~/.ssh/config`) are supported. `no-mistakes` resolves the alias through `ssh -G` to its real host name and uses that host only for provider detection and for scoping the provider CLI (`gh`/`glab`) to the right instance. The original Git remote URL is left untouched, so authentication and pushes continue to use the alias exactly as your SSH configuration expects.

If `ssh -G` is unavailable or the alias does not resolve, detection falls back to the literal host in the remote URL rather than failing the run.

## Unsupported hosts

If your upstream isn't GitHub, GitLab, Bitbucket Cloud, or Azure DevOps:

- The **push** step still runs - `no-mistakes` pushes through git to the configured target like any other remote.
- The **PR** step marks itself as `skipped`.
- The **CI** step marks itself as `skipped`.

Everything before push (rebase, review, test, document, lint) still works regardless of host. If your host has a CLI that exposes CI status and PR state, open an issue - new providers are straightforward to add.

## Checking what's wired up

```sh
no-mistakes doctor
```

`doctor` checks `gh` and `az` availability. For Bitbucket Cloud, it reports complete direct REST credentials first (without displaying them), otherwise verifies that an installed `bkt` is v0.30.0+ with a Cloud Keychain context; partial direct credentials remain an explicit warning. For GitLab, confirm `glab` is installed and authenticated. For Azure DevOps, confirm the `azure-devops` extension is installed (`az extension show --name azure-devops`) and a PAT is available.

:::note
When the daemon runs through a managed service (launchd, systemd, Task Scheduler), it reloads environment from your login shell on macOS and Linux so provider tools and `NO_MISTAKES_BITBUCKET_*` vars are picked up, and it augments `PATH` with common binary directories. On macOS, `bkt` reads its credential from Keychain in the daemon process; No Mistakes does not copy it into the service environment. If credentials or PATH-derived tools are missing, check `~/.no-mistakes/logs/daemon.log` for a login-shell environment resolution warning. On Windows it reuses the current process environment.
:::
