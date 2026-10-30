package db

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Nonconvergence is the latest successful advisory measurement, not a verdict.
type Nonconvergence struct {
	Probability float64
	Model       string
	Step        string
	Round       int
	Themes      *float64
	ObservedAt  int64
}

func (r *Run) Nonconvergence() *Nonconvergence {
	if r == nil || r.NonconvergenceProbability == nil {
		return nil
	}
	nc := &Nonconvergence{Probability: *r.NonconvergenceProbability, Themes: r.NonconvergenceThemes}
	if r.NonconvergenceModel != nil {
		nc.Model = *r.NonconvergenceModel
	}
	if r.NonconvergenceStep != nil {
		nc.Step = *r.NonconvergenceStep
	}
	if r.NonconvergenceRound != nil {
		nc.Round = *r.NonconvergenceRound
	}
	if r.NonconvergenceObservedAt != nil {
		nc.ObservedAt = *r.NonconvergenceObservedAt
	}
	return nc
}

// RecordRunNonconvergence intentionally does not touch updated_at, status or
// step state. A failed measurement is never written as a fabricated zero.
func (d *DB) RecordRunNonconvergence(runID string, nc Nonconvergence) error {
	_, err := d.sql.Exec(`UPDATE runs SET nonconvergence_probability = ?, nonconvergence_model = ?, nonconvergence_step = ?, nonconvergence_round = ?, nonconvergence_themes = ?, nonconvergence_observed_at = ? WHERE id = ?`, nc.Probability, nc.Model, nc.Step, nc.Round, nc.Themes, nc.ObservedAt, runID)
	if err != nil {
		return fmt.Errorf("record run nonconvergence: %w", err)
	}
	return nil
}

func (d *DB) UpdateRunTestVerifiedTreeSHA(id, treeSHA string) error {
	_, err := d.sql.Exec(`UPDATE runs SET test_verified_tree_sha = ?, updated_at = ? WHERE id = ?`, treeSHA, now(), id)
	if err != nil {
		return fmt.Errorf("update run test-verified tree: %w", err)
	}
	return nil
}

func (d *DB) TerminalPRCompletionCandidates() ([]*Run, error) {
	rows, err := d.sql.Query(`SELECT `+runColumns+` FROM runs WHERE status IN (?, ?) AND pr_state IN ('merged', 'closed')`, types.RunPending, types.RunRunning)
	if err != nil {
		return nil, fmt.Errorf("list terminal PR completion candidates: %w", err)
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		run := &Run{}
		if err := scanRun(rows, run); err != nil {
			return nil, fmt.Errorf("scan terminal PR completion candidate: %w", err)
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// CompleteSuccessfulRun is called only after the resource-cleanup barrier.
func (d *DB) CompleteSuccessfulRun(id string, completeTerminalPRStep bool) error {
	ts := now()
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("complete successful run: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if completeTerminalPRStep {
		if err := finalizeTerminalPRRun(tx, id, ts); err != nil {
			return fmt.Errorf("complete successful run: %w", err)
		}
	} else if _, err := tx.Exec(`UPDATE runs SET status = ?, push_active = 0, updated_at = ? WHERE id = ?`, types.RunCompleted, ts, id); err != nil {
		return fmt.Errorf("complete successful run: %w", err)
	}
	return tx.Commit()
}

const RunCustodyDiagnosticMarker = "worktree retained for custody recovery:"

// RecordRunCustodyDiagnostic retains the original terminal cause and custody
// evidence when cleanup refuses, rather than reporting a false successful exit.
func (d *DB) RecordRunCustodyDiagnostic(id, msg string) error {
	_, err := d.sql.Exec(`UPDATE runs SET error = CASE WHEN COALESCE(error, '') = '' THEN ? WHEN instr(error, ?) > 0 THEN error ELSE error || ' | ' || ? END,
	status = CASE WHEN status IN ('completed', 'failed', 'cancelled') THEN status ELSE ? END,
	push_active = 0, updated_at = ? WHERE id = ?`, msg, msg, msg, types.RunFailed, now(), id)
	return err
}
