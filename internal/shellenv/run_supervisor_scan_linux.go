//go:build linux

package shellenv

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// discoverWorktreeProcessGroups finds process groups that must be reaped before
// the run worktree is deleted. A candidate has to satisfy both halves of the
// proof: its cwd is inside the worktree, and its environment carries the run
// marker owner accepts. cwd alone would target a developer's shell; the marker
// alone would target a run process that legitimately moved elsewhere.
func discoverWorktreeProcessGroups(workDir string, owner runOwnership) []int {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" || owner.runID == "" {
		return nil
	}
	workDir = filepath.Clean(workDir)
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	seen := make(map[int]struct{})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if err != nil {
			continue
		}
		cwd = strings.TrimSuffix(cwd, " (deleted)")
		cwd = filepath.Clean(cwd)
		if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
			cwd = resolved
		}
		if !pathInside(cwd, workDir) {
			continue
		}
		if !owner.owns(readProcEnviron(entry.Name())) {
			continue
		}
		group, err := syscall.Getpgid(pid)
		if err != nil || group <= 1 || group == syscall.Getpgrp() {
			continue
		}
		seen[group] = struct{}{}
	}
	groups := make([]int, 0, len(seen))
	for group := range seen {
		groups = append(groups, group)
	}
	return groups
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

func pathInside(path, root string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != "" && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}
