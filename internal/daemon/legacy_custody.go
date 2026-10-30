package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// migrateLegacyPreservedHeads transfers only exact, durable fork authority to
// upstream's verified-terminal lifecycle. Crash refs and mismatched run refs
// remain evidence, never a source from which to choose a newer head. All old
// refs remain untouched, even after successful or partially failed migration.
func migrateLegacyPreservedHeads(ctx context.Context, d *db.DB, p *paths.Paths) error {
	repos, err := d.GetRepos()
	if err != nil {
		return err
	}
	var failures []error
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		gateDir := filepath.Clean(p.RepoDir(repo.ID))
		if filepath.Dir(gateDir) != filepath.Clean(p.ReposDir()) || filepath.Base(gateDir) != repo.ID+".git" {
			failures = append(failures, fmt.Errorf("unsafe legacy gate identity %q", repo.ID))
			continue
		}
		present, err := hasLegacyHeadRefs(gateDir)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		// Most gates have never run the fork. Keep their startup filesystem-only.
		if !present {
			continue
		}
		if !git.LooksLikeBareRepository(gateDir) {
			failures = append(failures, fmt.Errorf("invalid legacy gate %q", repo.ID))
			continue
		}
		if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
			failures = append(failures, err)
			continue
		}
		out, err := git.RunBare(ctx, gateDir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", "refs/no-mistakes/run-head/")
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if len(fields) != 2 {
				failures = append(failures, fmt.Errorf("refusing symbolic or malformed legacy authority %q", fields[0]))
				continue
			}
			id, ok := git.RunIDFromRunHeadRef(fields[0])
			if !ok {
				continue
			}
			run, err := d.GetRun(id)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if run == nil || run.RepoID != repo.ID || !run.Status.Terminal() || run.CustodyReturnedAt != nil || run.TerminalHeadVerifiedAt != nil || run.HeadSHA != fields[1] {
				continue
			}
			if err := migrateLegacyRunHead(ctx, d, gateDir, run); err != nil {
				failures = append(failures, fmt.Errorf("migrate legacy run %s: %w", id, err))
			}
		}
	}
	return errors.Join(failures...)
}

func hasLegacyHeadRefs(gateDir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(gateDir, "refs", "no-mistakes", "run-head")); err == nil {
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	packed, err := os.ReadFile(filepath.Join(gateDir, "packed-refs"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bytes.Contains(packed, []byte(" refs/no-mistakes/run-head/")), nil
}

func migrateLegacyRunHead(ctx context.Context, d *db.DB, gateDir string, run *db.Run) error {
	if err := git.VerifyExactRef(ctx, gateDir, git.RunHeadRef(run.ID), run.HeadSHA); err != nil {
		return err
	}
	if err := archiveCleanupHead(ctx, gateDir, run.ID, run.HeadSHA); err != nil {
		return err
	}
	crash, exists, err := git.ExactRefTarget(ctx, gateDir, git.CrashHeadRef(run.ID))
	if err != nil {
		return err
	}
	if exists {
		if err := archiveCleanupHead(ctx, gateDir, run.ID, crash); err != nil {
			return err
		}
	}
	// PreserveRecoveryHead is create-only/exact: a conflicting upstream anchor
	// is not overwritten, even when the legacy DB/ref pair agrees.
	if err := custody.PreserveRecoveryHead(ctx, gateDir, run.ID, run.HeadSHA); err != nil {
		return err
	}
	if err := git.VerifyExactRef(ctx, gateDir, git.RunHeadRef(run.ID), run.HeadSHA); err != nil {
		return err
	}
	changed, err := d.VerifyLegacyPreservedTerminalHead(run.ID, run.Status, run.HeadSHA)
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("run authority changed during migration; archives retained")
	}
	return nil
}
