package shellenv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const defaultRunSupervisorGrace = 2 * time.Second

const (
	// RunIDEnvVar and DaemonInstanceEnvVar stamp run ownership into every
	// subprocess a run launches. Process ancestry cannot prove ownership: an
	// escaped background process is by definition one whose command leader has
	// exited, so it has already been reparented to init and its ppid chain no
	// longer reaches the daemon. An inherited environment survives both
	// reparenting and a daemon restart, so it is the durable proof that a
	// process found sitting in a run worktree belongs to that run.
	RunIDEnvVar          = "NO_MISTAKES_RUN_ID"
	DaemonInstanceEnvVar = "NO_MISTAKES_DAEMON_INSTANCE"
)

// daemonInstanceID distinguishes this daemon process from any earlier one that
// stamped the same run marker. The PID alone is reusable across restarts, so
// the start timestamp is mixed in.
var daemonInstanceID = fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())

type runSupervisorContextKey struct{}

// RunSupervisor owns the process groups launched for one pipeline run. Commands
// registered with the supervisor remain associated with the run until the run
// lifecycle explicitly terminates the supervisor, so worktree cleanup has a
// single boundary that can reap background descendants before deleting cwd.
type RunSupervisor struct {
	runID   string
	workDir string
	grace   time.Duration

	// adoptPreviousInstances widens ownership to marked processes left behind
	// by an earlier daemon instance. Only startup orphan cleanup sets it.
	adoptPreviousInstances bool

	mu     sync.Mutex
	groups map[int]struct{}
}

// runOwnership is the proof a discovered process must satisfy before the
// supervisor may signal its group.
type runOwnership struct {
	runID                  string
	instanceID             string
	adoptPreviousInstances bool
}

// owns reports whether a process environment block proves the process belongs
// to this run. An unmarked process - a developer's shell or editor opened in a
// retained or custody worktree - never qualifies, whatever its cwd.
func (o runOwnership) owns(environ []byte) bool {
	if o.runID == "" || len(environ) == 0 {
		return false
	}
	runID, ok := environValue(environ, RunIDEnvVar)
	if !ok || runID == "" {
		return false
	}
	if runID == o.runID {
		return true
	}
	if !o.adoptPreviousInstances {
		return false
	}
	// A marked process from a different daemon instance belongs to a run that
	// instance can no longer be executing, so startup cleanup may reap it.
	instance, ok := environValue(environ, DaemonInstanceEnvVar)
	return ok && instance != "" && instance != o.instanceID
}

// environValue reads name from a NUL-separated environment block.
func environValue(environ []byte, name string) (string, bool) {
	prefix := name + "="
	for _, entry := range bytes.Split(environ, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		if text := string(entry); strings.HasPrefix(text, prefix) {
			return strings.TrimPrefix(text, prefix), true
		}
	}
	return "", false
}

// NewRunSupervisor creates a process-group supervisor for one run. workDir is
// used as an additional fail-safe during cleanup: a process group is discovered
// when one of its members both has a cwd inside the run worktree and carries
// this run's inherited environment marker, so a run child that escaped its
// command leader and reparented away is still reaped while unrelated daemon
// groups, sibling-run groups, and foreign processes that merely sit in the
// worktree (a developer's shell or editor inspecting a retained or custody
// worktree) are never signalled.
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

// NewOrphanRunSupervisor creates a supervisor for startup cleanup of a worktree
// left behind by a run that is no longer executing. It has no registered
// command groups to reap, so discovery is its only reach, and it additionally
// adopts marked processes stamped by an earlier daemon instance - every run of
// a previous instance is by definition finished.
func NewOrphanRunSupervisor(runID, workDir string) *RunSupervisor {
	supervisor := NewRunSupervisor(runID, workDir)
	supervisor.adoptPreviousInstances = true
	return supervisor
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
	for _, group := range discoverWorktreeProcessGroups(s.workDir, s.ownership()) {
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

func (s *RunSupervisor) ownership() runOwnership {
	if s == nil {
		return runOwnership{}
	}
	return runOwnership{
		runID:                  s.runID,
		instanceID:             daemonInstanceID,
		adoptPreviousInstances: s.adoptPreviousInstances,
	}
}

// applySupervisedRunEnv stamps the run marker into cmd's environment. It runs
// at StartShellCommand, the one boundary every run-owned command passes through
// after its caller has finished assembling cmd.Env, so no call site can drop the
// marker by assigning Env afterwards.
func applySupervisedRunEnv(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	value, ok := supervisedCommands.Load(cmd)
	if !ok {
		return
	}
	supervisor, _ := value.(*RunSupervisor)
	if supervisor == nil || supervisor.runID == "" {
		return
	}
	base := cmd.Env
	if base == nil {
		base = os.Environ()
	}
	env := make([]string, 0, len(base)+2)
	for _, entry := range base {
		if hasEnvName(entry, RunIDEnvVar) || hasEnvName(entry, DaemonInstanceEnvVar) {
			continue
		}
		env = append(env, entry)
	}
	cmd.Env = append(env,
		RunIDEnvVar+"="+supervisor.runID,
		DaemonInstanceEnvVar+"="+daemonInstanceID,
	)
}

func hasEnvName(entry, name string) bool {
	return strings.HasPrefix(entry, name+"=")
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
