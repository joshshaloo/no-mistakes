//go:build !linux

package shellenv

func discoverWorktreeProcessGroups(string, runOwnership) []int { return nil }
