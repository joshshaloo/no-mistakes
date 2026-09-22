package convergence

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Environment variable names the key is read from. These are the operator's
// existing, conventional names for this service; this package invents no new
// secret location of its own.
const (
	openRouterKeyEnv = "OPENROUTER_API_KEY"
	typeSafeKeyEnv   = "TYPESAFE_API_KEY"
)

// Defaults for the System One channel. DefaultModel is pinned rather than an
// alias so a recorded probability stays comparable across runs; an alias can
// move under a tuned threshold.
const (
	DefaultModel   = "jev-1.13"
	DefaultBaseURL = "https://openrouter.ai/api"
	DefaultTimeout = 10 * time.Second
	MaximumTimeout = 30 * time.Second
)

// Settings is the resolved, host-owned configuration for the detector. It is
// global-config only by design (see the package doc's trust-boundary note).
type Settings struct {
	// Enabled is the explicit opt-in. It defaults to false, so a host that has
	// configured nothing gets exactly today's behavior.
	Enabled bool
	// Model is the System One model id.
	Model string
	// BaseURL is the System One channel root; "/v1/systemone" is appended.
	BaseURL string
	// KeyFile is an existing operator-owned env file (KEY=VALUE lines) the key
	// is read from when it is not already in the environment. It is never a
	// command argument and its contents are never logged.
	KeyFile string
	// Timeout bounds one detector call end to end.
	Timeout time.Duration
}

// resolveKey returns the API key, or "" when none is configured. A missing key
// is not an error: it is one of the ordinary ways the detector stays silent.
func resolveKey(keyFile string) string {
	for _, name := range []string{openRouterKeyEnv, typeSafeKeyEnv} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	if strings.TrimSpace(keyFile) == "" {
		return ""
	}
	values, err := readEnvFile(expandHome(keyFile))
	if err != nil {
		return ""
	}
	for _, name := range []string{openRouterKeyEnv, typeSafeKeyEnv} {
		if v := strings.TrimSpace(values[name]); v != "" {
			return v
		}
	}
	return ""
}

// readEnvFile parses an operator-owned "KEY=VALUE" env file. It is deliberately
// minimal: blank lines and comments are skipped, an "export " prefix is
// tolerated, and one layer of surrounding quotes is stripped. Any read failure
// returns an error the caller turns into silence, never into a value.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		values[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

// expandHome resolves a leading "~/" against the current user's home so an
// operator can point key_file at the path they would type in a shell. This
// package owns key-file path semantics; a failure to resolve home leaves the
// path untouched and the open simply fails, which is silence, not a fallback.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

func unquote(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
