//go:build !linux && !darwin

package shellenv

func discoverRunProcesses(runOwnership, string) ([]discoveredProcess, error) { return nil, nil }
