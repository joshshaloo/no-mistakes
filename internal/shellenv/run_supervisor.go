package shellenv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultRunSupervisorGrace = 2 * time.Second

// RunIDEnvVar names the run-ownership marker stamped at every run-owned launch
// boundary and inherited by every descendant. It is the durable proof that a
// process belongs to a run, and the only thing that authorizes cleanup to
// signal it. Process ancestry cannot serve: an escaped background process is by
// definition one whose command leader has exited, so it has already been
// reparented to init and its ppid chain no longer reaches the daemon. An
// inherited environment survives that, and survives a daemon restart. A working
// directory cannot serve either - that a process is standing in the worktree
// says nothing about who started it - so cwd is diagnostic only.
const RunIDEnvVar = "NO_MISTAKES_RUN_ID"

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

// runOwnership is the proof a discovered process must satisfy before the
// supervisor may signal its group. Carrying this run's marker is the only proof
// there is: nothing else may authorize a kill.
type runOwnership struct {
	runID string
}

// ownsRun reports whether a process environment block carries this exact run's
// marker, and is the single ownership predicate for run-end cleanup and startup
// orphan cleanup alike. Run IDs are globally unique, so the marker is
// conclusive proof wherever the process has since moved and whichever daemon
// instance stamped it: a marked process that outlives its run has no legitimate
// reason to still be running, including one that daemonized with setsid plus
// chdir("/"). Everything else is spared - an unmarked process such as a
// developer's shell or editor opened in a retained or custody worktree, and
// equally a process carrying some *other* run's marker, which may still belong
// to a live run of a concurrently running daemon.
func (o runOwnership) ownsRun(environ []byte) bool {
	if o.runID == "" || len(environ) == 0 {
		return false
	}
	runID, ok := environValue(environ, RunIDEnvVar)
	return ok && runID == o.runID
}

// discoveredProcess is a marked process found by scanning the process table.
// cwd is diagnostic only - it never decides whether a run owns the process.
type discoveredProcess struct {
	pid        int
	group      int
	cwd        string
	inWorktree bool
}

// pathInside reports whether path is root or sits beneath it.
func pathInside(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != "" && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
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

// NewRunSupervisor creates a process-group supervisor for one run. Beyond the
// command groups it registers, cleanup discovers any process still carrying
// this run's inherited environment marker, so a child that escaped its command
// leader - reparented to init, in a session of its own, possibly having
// chdir'd out of the worktree - is still reaped, while unrelated daemon groups,
// sibling-run groups, and unmarked foreign processes (a developer's shell or
// editor inspecting a retained or custody worktree) are never signalled.
// workDir is retained for diagnostics in the termination log.
//
// Marker discovery is implemented on Linux only; on Windows the kill-on-close
// job object covers escaped descendants, and on other platforms cleanup is
// limited to the registered command groups.
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
	for _, proc := range discoverRunProcesses(s.ownership(), s.workDir) {
		if _, tracked := groups[proc.group]; !tracked {
			slog.Info("reaping run process that escaped its command group",
				"run_id", s.runID, "pid", proc.pid, "pgid", proc.group,
				"cwd", proc.cwd, "in_worktree", proc.inWorktree)
		}
		groups[proc.group] = struct{}{}
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
	return runOwnership{runID: s.runID}
}

// WithRunMarkers returns env with this run's ownership marker applied,
// replacing any inherited value so a stale marker can never win. It is the
// single owner of the marker name and value; every launch boundary that starts
// a run-owned process routes through it.
func (s *RunSupervisor) WithRunMarkers(env []string) []string {
	if s == nil || s.runID == "" {
		return env
	}
	if env == nil {
		env = os.Environ()
	}
	marked := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if hasEnvName(entry, RunIDEnvVar) {
			continue
		}
		marked = append(marked, entry)
	}
	return append(marked, RunIDEnvVar+"="+s.runID)
}

// ApplyRunMarkers stamps the marker of the run carried by ctx onto env. It is
// for launch boundaries that build their own process environment and do not go
// through StartShellCommand, such as the managed agent server. Without a
// RunSupervisor in ctx the environment is returned unchanged.
func ApplyRunMarkers(ctx context.Context, env []string) []string {
	return RunSupervisorFromContext(ctx).WithRunMarkers(env)
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
	cmd.Env = supervisor.WithRunMarkers(cmd.Env)
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
