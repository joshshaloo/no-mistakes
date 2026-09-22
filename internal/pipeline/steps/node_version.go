package steps

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// declaredNodeVersion reads the repository's own pinned Node version, checking
// sources in the order the Node tooling ecosystem treats as authoritative:
// .nvmrc, then .node-version, then a mise/asdf .tool-versions line, then
// package.json's "volta"."node", then package.json's "engines"."node" (only
// when it names an exact version rather than a range, which we cannot resolve
// deterministically). It returns ("", "", nil) when the repository declares no
// pin at all - that is not an error, it just means there is nothing to enforce.
func declaredNodeVersion(workDir string) (version, source string, err error) {
	type fileSource struct {
		path      string
		source    string
		extractor func(string) (string, bool)
	}
	plain := func(content string) (string, bool) {
		v := strings.TrimSpace(content)
		if v == "" {
			return "", false
		}
		return v, true
	}
	sources := []fileSource{
		{filepath.Join(workDir, ".nvmrc"), ".nvmrc", plain},
		{filepath.Join(workDir, ".node-version"), ".node-version", plain},
		{filepath.Join(workDir, ".tool-versions"), ".tool-versions", toolVersionsNodeLine},
	}
	for _, fs := range sources {
		content, readErr := os.ReadFile(fs.path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			return "", "", fmt.Errorf("read %s: %w", fs.source, readErr)
		}
		if v, ok := fs.extractor(string(content)); ok {
			return v, fs.source, nil
		}
	}

	pkgPath := filepath.Join(workDir, "package.json")
	pkgBytes, readErr := os.ReadFile(pkgPath)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("read package.json: %w", readErr)
	}
	var pkg struct {
		Volta struct {
			Node string `json:"node"`
		} `json:"volta"`
		Engines struct {
			Node string `json:"node"`
		} `json:"engines"`
	}
	if err := json.Unmarshal(pkgBytes, &pkg); err != nil {
		return "", "", fmt.Errorf("parse package.json: %w", err)
	}
	if v := strings.TrimSpace(pkg.Volta.Node); v != "" {
		return v, "package.json (volta.node)", nil
	}
	if v := strings.TrimSpace(pkg.Engines.Node); v != "" {
		return v, "package.json (engines.node)", nil
	}
	return "", "", nil
}

func toolVersionsNodeLine(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "node" || fields[0] == "nodejs" {
			return fields[1], true
		}
	}
	return "", false
}

var exactVersionPattern = regexp.MustCompile(`^\d+(\.\d+){0,2}$`)

// normalizeExactVersionParts accepts a full or partial numeric version
// declaration such as "20.19.0", "v20.19.0", or "20" and returns its numeric
// components. Partial declarations resolve to the highest matching installed
// version. Anything with range operators (^, ~, >=, x, *) or a named alias
// ("lts/*", "system", "latest") returns ok=false.
func normalizeExactVersionParts(raw string) (parts []int, ok bool) {
	v := strings.TrimSpace(raw)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	if !exactVersionPattern.MatchString(v) {
		return nil, false
	}
	for _, seg := range strings.Split(v, ".") {
		n, err := strconv.Atoi(seg)
		if err != nil {
			return nil, false
		}
		parts = append(parts, n)
	}
	return parts, true
}

func versionPartsString(parts []int) string {
	strs := make([]string, len(parts))
	for i, p := range parts {
		strs[i] = strconv.Itoa(p)
	}
	return strings.Join(strs, ".")
}

// versionSatisfies reports whether installed (a full x.y.z version) matches
// declared (which may be a full or partial prefix, e.g. declared "20" matches
// installed "20.19.0").
func versionSatisfies(declared, installed []int) bool {
	if len(declared) > len(installed) {
		return false
	}
	for i, d := range declared {
		if installed[i] != d {
			return false
		}
	}
	return true
}

func compareVersionParts(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

// nodeVersionManager describes one Node version manager's on-disk install
// layout so findPinnedNodeInstall can search it without depending on the
// manager's own CLI being on PATH (a pipeline step running under a
// version-manager shim cannot assume the shim itself is active).
type nodeVersionManager struct {
	name          string
	installsDir   func() (string, bool) // returns install root, and false if the manager is not configured on this host
	dirHasVPrefix bool
	binSubpath    []string // path segments from the version directory to its bin directory
}

func homeDir() (string, bool) {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return "", false
	}
	return h, true
}

func envOrHomeSubpath(envVar string, homeSubpath ...string) func() (string, bool) {
	return func() (string, bool) {
		if v := os.Getenv(envVar); v != "" {
			return v, true
		}
		h, ok := homeDir()
		if !ok {
			return "", false
		}
		return filepath.Join(append([]string{h}, homeSubpath...)...), true
	}
}

func nodeVersionManagers() []nodeVersionManager {
	return []nodeVersionManager{
		{
			name: "mise",
			installsDir: func() (string, bool) {
				root, ok := envOrHomeSubpath("MISE_DATA_DIR", ".local", "share", "mise")()
				if !ok {
					return "", false
				}
				return filepath.Join(root, "installs", "node"), true
			},
			binSubpath: []string{"bin"},
		},
		{
			name: "nvm",
			installsDir: func() (string, bool) {
				root, ok := envOrHomeSubpath("NVM_DIR", ".nvm")()
				if !ok {
					return "", false
				}
				return filepath.Join(root, "versions", "node"), true
			},
			dirHasVPrefix: true,
			binSubpath:    []string{"bin"},
		},
		{
			name: "volta",
			installsDir: func() (string, bool) {
				root, ok := envOrHomeSubpath("VOLTA_HOME", ".volta")()
				if !ok {
					return "", false
				}
				return filepath.Join(root, "tools", "image", "node"), true
			},
			binSubpath: []string{"bin"},
		},
		{
			name: "fnm",
			installsDir: func() (string, bool) {
				root, ok := envOrHomeSubpath("FNM_DIR", ".local", "share", "fnm")()
				if !ok {
					return "", false
				}
				return filepath.Join(root, "node-versions"), true
			},
			dirHasVPrefix: true,
			binSubpath:    []string{"installation", "bin"},
		},
		{
			name: "asdf",
			installsDir: func() (string, bool) {
				root, ok := envOrHomeSubpath("ASDF_DATA_DIR", ".asdf")()
				if !ok {
					return "", false
				}
				return filepath.Join(root, "installs", "nodejs"), true
			},
			binSubpath: []string{"bin"},
		},
	}
}

func nodeBinaryName() string {
	if isWindowsExec() {
		return "node.exe"
	}
	return "node"
}

// findPinnedNodeInstall searches every known Node version manager's install
// directory for a Node build matching declaredParts, and returns the bin
// directory of the highest matching version found. It never invokes a
// version-manager CLI and never installs anything - it only looks at what is
// already on disk, so it stays fast and side-effect free.
func findPinnedNodeInstall(declaredParts []int) (binDir, matchedVersion, managerName string, checked []string, err error) {
	type candidate struct {
		parts   []int
		binDir  string
		manager string
	}
	var best *candidate

	for _, mgr := range nodeVersionManagers() {
		root, ok := mgr.installsDir()
		if !ok {
			continue
		}
		checked = append(checked, fmt.Sprintf("%s (%s)", mgr.name, root))
		entries, readErr := os.ReadDir(root)
		if readErr != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			versionStr := name
			if mgr.dirHasVPrefix {
				versionStr = strings.TrimPrefix(versionStr, "v")
			}
			parts, ok := normalizeExactVersionParts(versionStr)
			if !ok || !versionSatisfies(declaredParts, parts) {
				continue
			}
			binDirPath := filepath.Join(append([]string{root, name}, mgr.binSubpath...)...)
			nodeBin := filepath.Join(binDirPath, nodeBinaryName())
			if fi, statErr := os.Stat(nodeBin); statErr != nil || !pathCandidateUsable(runtime.GOOS, nodeBin, fi) {
				continue
			}
			cand := candidate{parts: parts, binDir: binDirPath, manager: mgr.name}
			if best == nil || compareVersionParts(cand.parts, best.parts) > 0 {
				best = &cand
			}
		}
	}

	if best == nil {
		return "", "", "", checked, fmt.Errorf("no installed Node matching %s found", versionPartsString(declaredParts))
	}
	return best.binDir, versionPartsString(best.parts), best.manager, checked, nil
}

func pathNodeVersion(sctx *pipeline.StepContext) (parts []int, nodePath string, ok bool) {
	if len(sctx.Env) > 0 {
		nodePath = findInCustomPath(sctx.WorkDir, sctx.Env, nodeBinaryName())
		if nodePath == "" {
			if _, hasCustomPath := envValue(sctx.Env, "PATH"); hasCustomPath {
				return nil, "", false
			}
		}
	}
	if nodePath == "" {
		var err error
		nodePath, err = exec.LookPath(nodeBinaryName())
		if err != nil {
			return nil, "", false
		}
	}
	absolutePath, err := filepath.Abs(nodePath)
	if err != nil {
		return nil, "", false
	}
	fi, err := os.Stat(absolutePath)
	if err != nil || !pathCandidateUsable(runtime.GOOS, absolutePath, fi) {
		return nil, "", false
	}
	cmd := stepCmd(sctx, absolutePath, "--version")
	out, err := shellenv.OutputShellCommand(cmd)
	if err != nil {
		return nil, "", false
	}
	parts, ok = normalizeExactVersionParts(strings.TrimSpace(string(out)))
	return parts, absolutePath, ok
}

// nodeVersionOverride resolves the environment override needed to run the
// worktree's tooling under its declared Node version. A full pin may use the
// first PATH executable only after a supervised version probe proves an exact
// match. Otherwise full and partial numeric pins resolve against executable
// manager installs, with partial pins choosing the highest match.
//
// It returns (nil, "", nil) when the repository declares no Node pin. Every
// successful resolution returns a note naming the declaration source and exact
// selected version. Ranges, aliases, and unmatched pins return an error so the
// caller cannot silently fall back to another Node.
func nodeVersionOverride(sctx *pipeline.StepContext) (env []string, note string, err error) {
	declared, source, err := declaredNodeVersion(sctx.WorkDir)
	if err != nil {
		return nil, "", fmt.Errorf("determine repository Node pin: %w", err)
	}
	if declared == "" {
		return nil, "", nil
	}

	declaredParts, ok := normalizeExactVersionParts(declared)
	if !ok {
		return nil, "", fmt.Errorf(
			"repository pins Node to %q via %s, but this is a range or alias no-mistakes cannot resolve to one exact installed version; refusing to run tests on an unverified Node version rather than silently using the host default",
			declared, source)
	}

	if len(declaredParts) == 3 {
		if hostParts, hostPath, ok := pathNodeVersion(sctx); ok && compareVersionParts(declaredParts, hostParts) == 0 {
			return nil, fmt.Sprintf("resolved repository Node pin %q from %s to exact v%s using executable %s", declared, source, versionPartsString(hostParts), hostPath), nil
		}
	}

	binDir, matched, manager, checked, findErr := findPinnedNodeInstall(declaredParts)
	if findErr != nil {
		return nil, "", fmt.Errorf(
			"repository pins Node to %s (via %s), but no installed Node matches it on this host (checked: %s); refusing to run tests on the host's default Node instead of the pinned version",
			declared, source, strings.Join(checked, ", "))
	}

	currentPath := os.Getenv("PATH")
	if stepPath, ok := envValue(sctx.Env, "PATH"); ok {
		currentPath = stepPath
	}
	newPath := binDir
	if currentPath != "" {
		newPath = binDir + string(os.PathListSeparator) + currentPath
	}
	return []string{"PATH=" + newPath}, fmt.Sprintf("resolved repository Node pin %q from %s to exact v%s from %s", declared, source, matched, manager), nil
}

func runConfiguredTestCommand(sctx *pipeline.StepContext, testCmd string) (string, int, error) {
	nodeEnv, nodeNote, err := nodeVersionOverride(sctx)
	if err != nil {
		return "", -1, fmt.Errorf("resolve repository-pinned Node version: %w", err)
	}
	if nodeNote != "" {
		sctx.Log(nodeNote)
	}
	return runConfiguredStepShellCommandWithExtraEnv(sctx, configuredCommandTest, testCmd, nodeEnv)
}

func isWindowsExec() bool {
	return runtime.GOOS == "windows"
}
