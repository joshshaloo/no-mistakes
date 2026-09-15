//go:build linux

package shellenv

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// discoverRunProcesses finds processes that must be reaped before the run
// worktree is deleted. Ownership is proven solely by this run's inherited
// marker in /proc/<pid>/environ, not by where the process happens to be
// standing: a daemonized dashboard that called setsid and chdir("/") is still
// this run's to clean up, while a process merely sitting in the worktree is
// not. The worktree path is recorded for the termination log only.
func discoverRunProcesses(owner runOwnership, workDir string) []discoveredProcess {
	if owner.runID == "" {
		return nil
	}
	workDir = strings.TrimSpace(workDir)
	if workDir != "" {
		workDir = filepath.Clean(workDir)
		if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
			workDir = resolved
		}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	selfGroup := syscall.Getpgrp()
	seen := make(map[int]struct{})
	var found []discoveredProcess
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		if !owner.ownsRun(readProcEnviron(entry.Name())) {
			continue
		}
		cwd, inWorktree := processCwd(entry.Name(), workDir)
		group, err := syscall.Getpgid(pid)
		if err != nil || group <= 1 || group == selfGroup {
			continue
		}
		if _, duplicate := seen[group]; duplicate {
			continue
		}
		seen[group] = struct{}{}
		found = append(found, discoveredProcess{pid: pid, group: group, cwd: cwd, inWorktree: inWorktree})
	}
	return found
}

// readProcEnviron returns the process environment block, or nil when it cannot
// be read. /proc/<pid>/environ is readable only by the same user, so a process
// owned by someone else fails closed and is never signalled.
func readProcEnviron(pid string) []byte {
	environ, err := os.ReadFile(filepath.Join("/proc", pid, "environ"))
	if err != nil {
		return nil
	}
	return environ
}

func processCwd(pid, workDir string) (string, bool) {
	cwd, err := os.Readlink(filepath.Join("/proc", pid, "cwd"))
	if err != nil {
		return "", false
	}
	cwd = filepath.Clean(strings.TrimSuffix(cwd, " (deleted)"))
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	return cwd, pathInside(cwd, workDir)
}
