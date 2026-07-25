package git

import (
	"context"
	"fmt"
	"strings"
)

const (
	runHeadRefPrefix   = "refs/no-mistakes/run-head/"
	crashHeadRefPrefix = "refs/no-mistakes/crash-head/"
)

// CommandRunner is the minimal Git command surface used by preservation
// publication. Pipeline call sites with a controlled step environment pass
// their runner so preservation commands inherit the same credentials, PATH,
// and non-interactive hardening as every other command in that step.
type CommandRunner func(args ...string) (string, error)

// RunHeadRef is the durable, run-owned gate reference for the exact pipeline
// head recorded as runs.head_sha. Unlike refs/heads/<branch>, it is independent
// run provenance: recovery and rerun may trust it only when it resolves
// byte-for-byte to the database value.
func RunHeadRef(runID string) string { return runHeadRefPrefix + runID }

// CrashHeadRef names a live worktree head that differed from durable run
// authority during cleanup. Its presence is evidence of an ambiguous crash,
// never authority to promote or discard either head automatically.
func CrashHeadRef(runID string) string { return crashHeadRefPrefix + runID }

// ResolveExactCommit proves that sha names a commit in repoDir's object
// database and that Git resolves it byte-for-byte to the supplied object ID.
func ResolveExactCommit(ctx context.Context, repoDir, sha string) (string, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "", fmt.Errorf("commit SHA is empty")
	}
	resolved, err := Run(ctx, repoDir, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve exact commit %s: %w", sha, err)
	}
	if resolved != sha {
		return "", fmt.Errorf("resolved commit %s does not exactly equal %s", resolved, sha)
	}
	return resolved, nil
}

// PinExactCommit verifies sha in the managed object database, updates ref to
// that exact commit, and re-reads the ref. Callers must not treat a successful
// update-ref alone as proof because a malformed or symbolic ref is not an
// acceptable custody anchor.
func PinExactCommit(ctx context.Context, repoDir, ref, sha string) error {
	return PinExactCommitWithRunner(defaultCommandRunner(ctx, repoDir), ref, sha)
}

func PinExactCommitWithRunner(run CommandRunner, ref, sha string) error {
	if err := validatePrivateRefWithRunner(run, ref); err != nil {
		return err
	}
	if _, err := resolveExactCommitWithRunner(run, sha); err != nil {
		return err
	}
	if _, err := run("update-ref", ref, sha); err != nil {
		return fmt.Errorf("pin exact commit at %s: %w", ref, err)
	}
	return verifyExactRefWithRunner(run, ref, sha)
}

// PinRunHead creates or advances the exact run-owned preservation ref.
func PinRunHead(ctx context.Context, repoDir, runID, sha string) error {
	return PinExactCommit(ctx, repoDir, RunHeadRef(runID), sha)
}

// VerifyExactRef requires ref to resolve byte-for-byte to sha.
func VerifyExactRef(ctx context.Context, repoDir, ref, sha string) error {
	resolved, err := ResolveRef(ctx, repoDir, ref)
	if err != nil {
		return err
	}
	if resolved != sha {
		return fmt.Errorf("ref %s resolves to %s, not exact commit %s", ref, resolved, sha)
	}
	return nil
}

// PublishRunHead centralizes Git publication ordering for a pipeline-created
// head. It first pins newSHA under the run-owned ref, then moves the mutable
// branch only with compare-and-swap from expectedOld. Thus a branch race never
// gets clobbered and the pipeline-created commit remains named for diagnosis.
// Database and in-memory run authority must be advanced only after this returns.
func PublishRunHead(ctx context.Context, repoDir, runID, branch, expectedOld, newSHA string) error {
	return PublishRunHeadWithRunner(defaultCommandRunner(ctx, repoDir), runID, branch, expectedOld, newSHA)
}

func PublishRunHeadWithRunner(run CommandRunner, runID, branch, expectedOld, newSHA string) error {
	if err := PinExactCommitWithRunner(run, RunHeadRef(runID), newSHA); err != nil {
		return fmt.Errorf("preserve pipeline head: %w", err)
	}
	branchRef := strings.TrimSpace(branch)
	if !strings.HasPrefix(branchRef, "refs/heads/") {
		branchRef = "refs/heads/" + strings.TrimPrefix(branchRef, "refs/heads/")
	}
	if _, err := run("check-ref-format", branchRef); err != nil {
		return fmt.Errorf("invalid pipeline branch ref %q: %w", branchRef, err)
	}
	if _, err := resolveExactCommitWithRunner(run, expectedOld); err != nil {
		return fmt.Errorf("verify expected branch head: %w", err)
	}
	current, err := resolveRefWithRunner(run, branchRef)
	if err != nil {
		return fmt.Errorf("resolve mutable pipeline branch: %w", err)
	}
	// Idempotent equality is not a branch move. It occurs in focused tests that
	// run a rebase on an attached branch; production pipeline worktrees are
	// detached, so their branch transition always takes the CAS below.
	if current != newSHA {
		if _, err := run("update-ref", branchRef, newSHA, expectedOld); err != nil {
			return fmt.Errorf("publish pipeline head compare-and-swap from %s to %s: %w", expectedOld, newSHA, err)
		}
	}
	if err := verifyExactRefWithRunner(run, branchRef, newSHA); err != nil {
		return fmt.Errorf("verify published pipeline branch: %w", err)
	}
	return nil
}

// FetchRemoteRefToPrivateRef fetches one exact private ref without touching
// FETCH_HEAD or ordinary remote-tracking refs.
func FetchRemoteRefToPrivateRef(ctx context.Context, dir, remote, remoteRef, localRef string) error {
	if err := validatePrivateRef(ctx, dir, remoteRef); err != nil {
		return fmt.Errorf("invalid remote preservation ref: %w", err)
	}
	if err := validatePrivateRef(ctx, dir, localRef); err != nil {
		return fmt.Errorf("invalid local preservation ref: %w", err)
	}
	_, err := Run(ctx, dir, "fetch", "--no-tags", "--no-write-fetch-head", remote, "+"+remoteRef+":"+localRef)
	return err
}

func validatePrivateRef(ctx context.Context, repoDir, ref string) error {
	return validatePrivateRefWithRunner(defaultCommandRunner(ctx, repoDir), ref)
}

func validatePrivateRefWithRunner(run CommandRunner, ref string) error {
	if !strings.HasPrefix(ref, "refs/no-mistakes/") {
		return fmt.Errorf("ref %q is outside refs/no-mistakes", ref)
	}
	if _, err := run("check-ref-format", ref); err != nil {
		return fmt.Errorf("invalid private ref %q: %w", ref, err)
	}
	return nil
}

func defaultCommandRunner(ctx context.Context, repoDir string) CommandRunner {
	return func(args ...string) (string, error) { return Run(ctx, repoDir, args...) }
}

func resolveExactCommitWithRunner(run CommandRunner, sha string) (string, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "", fmt.Errorf("commit SHA is empty")
	}
	resolved, err := run("rev-parse", "--verify", "--quiet", sha+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve exact commit %s: %w", sha, err)
	}
	if resolved != sha {
		return "", fmt.Errorf("resolved commit %s does not exactly equal %s", resolved, sha)
	}
	return resolved, nil
}

func resolveRefWithRunner(run CommandRunner, ref string) (string, error) {
	resolved, err := run("rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve ref %s: %w", ref, err)
	}
	return resolved, nil
}

func verifyExactRefWithRunner(run CommandRunner, ref, sha string) error {
	resolved, err := resolveRefWithRunner(run, ref)
	if err != nil {
		return err
	}
	if resolved != sha {
		return fmt.Errorf("ref %s resolves to %s, not exact commit %s", ref, resolved, sha)
	}
	return nil
}
