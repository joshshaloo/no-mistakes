//go:build linux

package shellenv

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// maxProcessAncestryDepth bounds the /proc ppid walk so a malformed or racing
// ancestry chain can never spin.
const maxProcessAncestryDepth = 64

func discoverWorktreeProcessGroups(workDir string) []int {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
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
	daemonPID := os.Getpid()
	seen := make(map[int]struct{})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == daemonPID {
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
		// A shared cwd alone never authorizes a kill: an interactive shell or
		// editor a developer opened in a retained or custody worktree also
		// matches. Only processes that descend from this daemon are inside the
		// run's own process tree.
		if !descendsFrom(pid, daemonPID) {
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

func descendsFrom(pid, ancestor int) bool {
	if ancestor <= 0 {
		return false
	}
	for depth := 0; depth < maxProcessAncestryDepth; depth++ {
		if pid == ancestor {
			return true
		}
		if pid <= 1 {
			return false
		}
		parent, ok := parentPID(pid)
		if !ok || parent == pid {
			return false
		}
		pid = parent
	}
	return false
}

// parentPID reads field 4 (ppid) of /proc/<pid>/stat. The comm field can itself
// contain spaces and parentheses, so parsing starts after its final ')'.
func parentPID(pid int) (int, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 || end+1 >= len(data) {
		return 0, false
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 2 {
		return 0, false
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return parent, true
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
