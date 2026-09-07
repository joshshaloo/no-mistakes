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
// used as an additional fail-safe during cleanup: a process group is discovered
// when one of its members both has a cwd inside the run worktree and descends
// from this daemon process, so a run child that escaped its command leader is
// still reaped while unrelated daemon groups, sibling-run groups, and foreign
// processes that merely sit in the worktree (a developer's shell or editor
// inspecting a retained or custody worktree) are never signalled.
//
// The cwd-based discovery fail-safe is implemented on Linux only; on Windows
// the kill-on-close job object covers escaped descendants, and on other
// platforms cleanup is limited to the registered command groups.
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

// ConfigureShellCommandForContext prepares cmd for process-tree cleanup and
// registers its process group with the RunSupervisor carried by ctx once
// StartShellCommand succeeds.
//
// When ctx carries no supervisor the command is deliberately left in the
// caller's process group. Short-lived Git and SCM subprocesses are also invoked
// straight from the CLI and the TUI, which install no signal handler: moving
// them into their own group would stop a terminal Ctrl-C from reaching them and
// leave an orphaned network fetch holding gate lock files. Only run-owned
// subprocesses get the process-group boundary.
//
// Long-lived subprocesses that must always be reaped as a tree regardless of
// who launched them (agents, configured repo commands) call ConfigureShellCommand
// directly and then SuperviseShellCommand to attach run ownership.
func ConfigureShellCommandForContext(ctx context.Context, cmd *exec.Cmd) {
	supervisor := RunSupervisorFromContext(ctx)
	if supervisor == nil {
		return
	}
	ConfigureShellCommand(cmd)
	supervisedCommands.Store(cmd, supervisor)
}

// SuperviseShellCommand attaches run ownership to a command that the caller has
// already prepared with ConfigureShellCommand, so the run supervisor can reap
// its process group at final cleanup. It is a no-op when ctx carries no
// RunSupervisor.
func SuperviseShellCommand(ctx context.Context, cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
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
