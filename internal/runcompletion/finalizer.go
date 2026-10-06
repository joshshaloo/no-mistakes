package runcompletion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type Finalizer struct {
	DB                     *db.DB
	Cleanup                func() error
	OnEvent                func(ipc.Event)
	CompleteTerminalPRStep bool
}

func (f Finalizer) Finalize(ctx context.Context, run *db.Run, repo *db.Repo) error {
	var cleanupErr error
	if f.Cleanup != nil {
		cleanupErr = f.Cleanup()
	}
	if cause := context.Cause(ctx); cause != nil {
		err := cause
		if cleanupErr != nil {
			err = errors.Join(cause, fmt.Errorf("complete run cleanup: %w", cleanupErr))
		}
		return f.fail(ctx, run, repo, err)
	}
	if cleanupErr != nil {
		return f.fail(ctx, run, repo, fmt.Errorf("complete run cleanup: %w", cleanupErr))
	}
	if err := f.DB.CompleteSuccessfulRun(run.ID, f.CompleteTerminalPRStep); err != nil {
		return f.fail(ctx, run, repo, fmt.Errorf("update run status: %w", err))
	}
	run.Status = types.RunCompleted
	f.emit(run, repo)
	return nil
}

func (f Finalizer) fail(ctx context.Context, run *db.Run, repo *db.Repo, err error) error {
	errMsg := err.Error()
	cancelReason := ""
	if cause := context.Cause(ctx); cause != nil && cause != context.Canceled {
		cancelReason = cause.Error()
		if !errors.Is(err, cause) {
			errMsg = cancelReason
		}
	}
	status := types.RunFailed
	if cancelReason == types.RunCancelReasonAbortedByUser || cancelReason == types.RunCancelReasonSuperseded ||
		(cancelReason == "" && (errMsg == types.RunCancelReasonAbortedByUser || errMsg == types.RunCancelReasonSuperseded)) {
		status = types.RunCancelled
	}
	if persisted, loadErr := f.DB.GetRun(run.ID); loadErr == nil && persisted != nil && persisted.Error != nil &&
		strings.Contains(*persisted.Error, db.RunCustodyDiagnosticMarker) && !strings.Contains(errMsg, db.RunCustodyDiagnosticMarker) {
		errMsg += " | " + *persisted.Error
	}
	if dbErr := f.DB.UpdateRunErrorStatus(run.ID, errMsg, status); dbErr != nil {
		slog.Error("failed to update run error status", "run", run.ID, "error", dbErr)
	}
	run.Status = status
	run.Error = &errMsg
	f.emit(run, repo)
	return err
}

func (f Finalizer) emit(run *db.Run, repo *db.Repo) {
	if f.OnEvent == nil {
		return
	}
	repoID := run.RepoID
	if repo != nil {
		repoID = repo.ID
	}
	status := string(run.Status)
	f.OnEvent(ipc.Event{
		Type: ipc.EventRunCompleted, RunID: run.ID, RepoID: repoID,
		Status: &status, Branch: &run.Branch, Error: run.Error, PRURL: run.PRURL,
	})
}
