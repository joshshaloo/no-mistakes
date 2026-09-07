//go:build linux

package shellenv

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

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
	seen := make(map[int]struct{})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == os.Getpid() {
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
