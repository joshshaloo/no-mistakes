package shellenv

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

const defaultRunSupervisorGrace = 2 * time.Second

type runSupervisorContextKey struct{}

// RunSupervisor owns the process groups launched for one pipeline run. Commands
// registered with the supervisor remain associated with the run until the run
// lifecycle explicitly terminates the supervisor, so worktree cleanup has a
// single boundary that can reap background descendants before deleting cwd.
type RunSupervisor struct {
	runID   string
	workDir string
	grace   time.Duration

	mu     sync.Mutex
	groups map[int]struct{}
}

// NewRunSupervisor creates a process-group supervisor for one run. workDir is
// used as an additional fail-safe during cleanup: processes whose cwd is inside
// the run worktree are discovered even if their command leader has already
// exited, while unrelated daemon or sibling-run groups are ignored.
func NewRunSupervisor(runID, workDir string) *RunSupervisor {
	return &RunSupervisor{
		runID:   runID,
		workDir: workDir,
		grace:   defaultRunSupervisorGrace,
		groups:  make(map[int]struct{}),
	}
}

// WithRunSupervisor returns a child context that marks subprocesses as owned by
// the supplied run supervisor.
func WithRunSupervisor(ctx context.Context, supervisor *RunSupervisor) context.Context {
	if ctx == nil || supervisor == nil {
		return ctx
	}
	return context.WithValue(ctx, runSupervisorContextKey{}, supervisor)
}

// RunSupervisorFromContext returns the run supervisor carried by ctx, if any.
func RunSupervisorFromContext(ctx context.Context) *RunSupervisor {
	if ctx == nil {
		return nil
	}
	supervisor, _ := ctx.Value(runSupervisorContextKey{}).(*RunSupervisor)
	return supervisor
}

// ConfigureShellCommandForContext prepares cmd for process-tree cleanup and, if
// ctx carries a RunSupervisor, registers the command's process group with that
// run after StartShellCommand succeeds.
func ConfigureShellCommandForContext(ctx context.Context, cmd *exec.Cmd) {
	ConfigureShellCommand(cmd)
	if supervisor := RunSupervisorFromContext(ctx); supervisor != nil {
		supervisedCommands.Store(cmd, supervisor)
	}
}

// Terminate sends SIGTERM (or the platform equivalent) to every process group
// owned by the run, waits for a bounded grace period, then forces survivors
// down before the caller removes the worktree.
func (s *RunSupervisor) Terminate(ctx context.Context) error {
	if s == nil {
		return nil
	}
	groups := s.snapshotGroups()
	for _, group := range discoverWorktreeProcessGroups(s.workDir) {
		groups[group] = struct{}{}
	}
	var errs []error
	for group := range groups {
		if err := terminateRunProcessGroup(ctx, group, s.grace); err != nil {
			errs = append(errs, fmt.Errorf("terminate run %s process group %d: %w", s.runID, group, err))
		}
	}
	s.clearGroups()
	return errors.Join(errs...)
}

func (s *RunSupervisor) trackGroup(group int) {
	if s == nil || group <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups[group] = struct{}{}
}

func (s *RunSupervisor) untrackGroup(group int) {
	if s == nil || group <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.groups, group)
}

func (s *RunSupervisor) snapshotGroups() map[int]struct{} {
	groups := make(map[int]struct{})
	if s == nil {
		return groups
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for group := range s.groups {
		groups[group] = struct{}{}
	}
	return groups
}

func (s *RunSupervisor) clearGroups() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.groups)
}

var supervisedCommands sync.Map // *exec.Cmd -> *RunSupervisor

func registerStartedShellCommand(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	value, ok := supervisedCommands.Load(cmd)
	if !ok {
		return
	}
	supervisor, _ := value.(*RunSupervisor)
	if supervisor == nil {
		return
	}
	group, err := shellCommandProcessGroup(cmd)
	if err != nil {
		group = cmd.Process.Pid
	}
	supervisor.trackGroup(group)
}

func unregisterShellCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	value, ok := supervisedCommands.LoadAndDelete(cmd)
	if !ok || cmd.Process == nil {
		return
	}
	supervisor, _ := value.(*RunSupervisor)
	if supervisor == nil {
		return
	}
	group, err := shellCommandProcessGroup(cmd)
	if err != nil {
		group = cmd.Process.Pid
	}
	supervisor.untrackGroup(group)
}
