//go:build !linux

package shellenv

func discoverWorktreeProcessGroups(string) []int { return nil }
