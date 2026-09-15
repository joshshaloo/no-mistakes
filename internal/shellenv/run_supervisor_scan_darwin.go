//go:build darwin

package shellenv

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const darwinProcessDiscoveryTimeout = 10 * time.Second

type darwinProcessInfo struct {
	ppid int
	pgid int
}

type darwinLsofProcess struct {
	pid  int
	pgid int
	cwd  string
}

var (
	darwinCurrentPID     = os.Getpid
	darwinCurrentPgrp    = syscall.Getpgrp
	darwinLookPath       = exec.LookPath
	darwinCommandContext = exec.CommandContext
)

// discoverRunProcesses finds escaped run processes on macOS before their
// worktree is removed. Darwin does not expose Linux's /proc/<pid>/environ
// marker scan without cgo/libproc, so this platform fallback intentionally uses
// lsof's cwd index and a daemon-descendant guard: a process must be standing in
// this run's worktree and still descend from this daemon before cleanup may
// signal its process group. If lsof is unavailable or fails unexpectedly,
// discovery returns an error so cleanup fails loud instead of silently skipping
// the fail-safe.
func discoverRunProcesses(owner runOwnership, workDir string) ([]discoveredProcess, error) {
	if owner.runID == "" {
		return nil, nil
	}
	workDir = cleanDiscoveryPath(workDir)
	if workDir == "" {
		return nil, nil
	}
	candidates, err := darwinLsofCWD(workDir)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	processes, err := darwinProcessTable()
	if err != nil {
		return nil, err
	}
	self := darwinCurrentPID()
	selfGroup := darwinCurrentPgrp()
	seen := make(map[int]struct{})
	var found []discoveredProcess
	for _, candidate := range candidates {
		if candidate.pid <= 1 || candidate.pid == self {
			continue
		}
		if !darwinDescendsFrom(candidate.pid, self, processes) {
			continue
		}
		cwd := cleanDiscoveryPath(candidate.cwd)
		if !pathInside(cwd, workDir) {
			continue
		}
		group := candidate.pgid
		if group <= 0 {
			if info, ok := processes[candidate.pid]; ok {
				group = info.pgid
			}
		}
		if group <= 1 || group == selfGroup {
			continue
		}
		if _, duplicate := seen[group]; duplicate {
			continue
		}
		seen[group] = struct{}{}
		found = append(found, discoveredProcess{pid: candidate.pid, group: group, cwd: cwd, inWorktree: true})
	}
	return found, nil
}

func cleanDiscoveryPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path
}

func darwinLsofCWD(workDir string) ([]darwinLsofProcess, error) {
	if _, err := darwinLookPath("lsof"); err != nil {
		return nil, fmt.Errorf("darwin escaped process discovery requires lsof in PATH: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), darwinProcessDiscoveryTimeout)
	defer cancel()
	cmd := darwinCommandContext(ctx, "lsof", "-n", "-a", "-d", "cwd", "-Fnpg", "+D", workDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("lsof cwd scan timed out for %s: %w", workDir, ctx.Err())
		}
		// lsof exits 1 when there are simply no matching processes. Treat that
		// specific empty result as success; every other failure is loud.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 && len(bytes.TrimSpace(stderr.Bytes())) == 0 {
			return nil, nil
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(string(out))
		}
		return nil, fmt.Errorf("lsof cwd scan for %s failed: %w: %s", workDir, err, message)
	}
	return parseDarwinLsof(out), nil
}

func parseDarwinLsof(out []byte) []darwinLsofProcess {
	var found []darwinLsofProcess
	current := darwinLsofProcess{}
	flush := func() {
		if current.pid > 0 && current.cwd != "" {
			found = append(found, current)
		}
	}
	for _, raw := range bytes.Split(out, []byte{'\n'}) {
		line := string(bytes.TrimSpace(raw))
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			flush()
			current = darwinLsofProcess{pid: atoiField(line[1:])}
		case 'g':
			current.pgid = atoiField(line[1:])
		case 'n':
			current.cwd = line[1:]
		}
	}
	flush()
	return found
}

func darwinProcessTable() (map[int]darwinProcessInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), darwinProcessDiscoveryTimeout)
	defer cancel()
	cmd := darwinCommandContext(ctx, "ps", "-axo", "pid=,ppid=,pgid=")
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ps process table scan timed out: %w", ctx.Err())
		}
		return nil, fmt.Errorf("ps process table scan failed: %w", err)
	}
	processes := make(map[int]darwinProcessInfo)
	for _, raw := range bytes.Split(out, []byte{'\n'}) {
		fields := strings.Fields(string(raw))
		if len(fields) < 3 {
			continue
		}
		pid := atoiField(fields[0])
		if pid <= 0 {
			continue
		}
		processes[pid] = darwinProcessInfo{ppid: atoiField(fields[1]), pgid: atoiField(fields[2])}
	}
	return processes, nil
}

func darwinDescendsFrom(pid, ancestor int, processes map[int]darwinProcessInfo) bool {
	if pid <= 1 || ancestor <= 1 || pid == ancestor {
		return false
	}
	seen := make(map[int]struct{})
	for pid > 1 {
		if _, loop := seen[pid]; loop {
			return false
		}
		seen[pid] = struct{}{}
		info, ok := processes[pid]
		if !ok {
			return false
		}
		if info.ppid == ancestor {
			return true
		}
		pid = info.ppid
	}
	return false
}

func atoiField(text string) int {
	value, _ := strconv.Atoi(strings.TrimSpace(text))
	return value
}
