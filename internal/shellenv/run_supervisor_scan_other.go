//go:build !linux

package shellenv

func discoverRunProcesses(runOwnership, string) []discoveredProcess { return nil }
