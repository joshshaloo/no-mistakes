package steps

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestDeclaredNodeVersion_NoPinIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	version, source, err := declaredNodeVersion(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != "" || source != "" {
		t.Fatalf("expected no declared pin, got version=%q source=%q", version, source)
	}
}

// TestDeclaredNodeVersion_FullPrecedenceChain locks in the complete fallback
// order (.nvmrc > .node-version > .tool-versions > package.json volta.node >
// package.json engines.node) end to end, one source at a time, so a future
// edit that reorders or skips a source is caught here rather than silently
// picking the wrong declared pin (and, transitively, reintroducing the
// original defect of running tests under an unintended Node).
func TestDeclaredNodeVersion_FullPrecedenceChain(t *testing.T) {
	dir := t.TempDir()

	// Every lower-precedence source is present from the start; each step
	// only adds the next higher-precedence source and re-checks the winner,
	// proving each source actually shadows the ones below it rather than
	// merely being the only one present.
	writeFile(t, filepath.Join(dir, "package.json"), `{"engines":{"node":"14.0.0"}}`)
	assertDeclaredNodeVersion(t, dir, "14.0.0", "package.json (engines.node)")

	writeFile(t, filepath.Join(dir, "package.json"), `{"volta":{"node":"15.0.0"},"engines":{"node":"14.0.0"}}`)
	assertDeclaredNodeVersion(t, dir, "15.0.0", "package.json (volta.node)")

	writeFile(t, filepath.Join(dir, ".tool-versions"), "nodejs 16.0.0\n")
	assertDeclaredNodeVersion(t, dir, "16.0.0", ".tool-versions")

	writeFile(t, filepath.Join(dir, ".node-version"), "18.0.0\n")
	assertDeclaredNodeVersion(t, dir, "18.0.0", ".node-version")

	writeFile(t, filepath.Join(dir, ".nvmrc"), "20.19.0\n")
	assertDeclaredNodeVersion(t, dir, "20.19.0", ".nvmrc")
}

func assertDeclaredNodeVersion(t *testing.T, dir, wantVersion, wantSource string) {
	t.Helper()
	version, source, err := declaredNodeVersion(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != wantVersion || source != wantSource {
		t.Fatalf("got version=%q source=%q, want version=%q source=%q", version, source, wantVersion, wantSource)
	}
}

func TestDeclaredNodeVersion_ToolVersionsLine(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".tool-versions"), "ruby 3.2.0\nnodejs 20.19.0\n")

	version, source, err := declaredNodeVersion(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != "20.19.0" || source != ".tool-versions" {
		t.Fatalf("got version=%q source=%q, want 20.19.0 from .tool-versions", version, source)
	}
}

func TestDeclaredNodeVersion_PackageJSONVoltaBeforeEngines(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"volta":{"node":"20.19.0"},"engines":{"node":"18.0.0"}}`)

	version, source, err := declaredNodeVersion(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if version != "20.19.0" || source != "package.json (volta.node)" {
		t.Fatalf("got version=%q source=%q, want volta pin", version, source)
	}
}

func TestDeclaredNodeVersion_UnrelatedInvalidPackageMetadataWithoutPinIsNoop(t *testing.T) {
	for _, content := range []string{
		`{"volta":[],"engines":42,"scripts":"unexpected"}`,
		`{"name":"broken",`,
	} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "package.json"), content)
		version, source, err := declaredNodeVersion(dir)
		if err != nil {
			t.Fatalf("package.json %q: unexpected error: %v", content, err)
		}
		if version != "" || source != "" {
			t.Fatalf("package.json %q: got version=%q source=%q, want no pin", content, version, source)
		}
	}
}

func TestNormalizeExactVersionParts(t *testing.T) {
	cases := []struct {
		in    string
		parts []int
		ok    bool
	}{
		{"20.19.0", []int{20, 19, 0}, true},
		{"v20.19.0", []int{20, 19, 0}, true},
		{"20", []int{20}, true},
		{"20.19", []int{20, 19}, true},
		{"^20.19.0", nil, false},
		{"~20.19.0", nil, false},
		{">=20.19.0", nil, false},
		{"20.x", nil, false},
		{"lts/*", nil, false},
		{"system", nil, false},
		{"", nil, false},
	}
	for _, tc := range cases {
		parts, ok := normalizeExactVersionParts(tc.in)
		if ok != tc.ok {
			t.Fatalf("normalizeExactVersionParts(%q) ok = %v, want %v", tc.in, ok, tc.ok)
		}
		if ok && !intSlicesEqual(parts, tc.parts) {
			t.Fatalf("normalizeExactVersionParts(%q) = %v, want %v", tc.in, parts, tc.parts)
		}
	}
}

func intSlicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestVersionSatisfies(t *testing.T) {
	if !versionSatisfies([]int{20}, []int{20, 19, 0}) {
		t.Fatal("expected major-only pin to match a full installed version")
	}
	if !versionSatisfies([]int{20, 19}, []int{20, 19, 0}) {
		t.Fatal("expected major.minor pin to match a full installed version")
	}
	if !versionSatisfies([]int{20, 19, 0}, []int{20, 19, 0}) {
		t.Fatal("expected exact pin to match itself")
	}
	if versionSatisfies([]int{20, 19, 1}, []int{20, 19, 0}) {
		t.Fatal("expected differing patch to not match")
	}
	if versionSatisfies([]int{20, 19, 0}, []int{20}) {
		t.Fatal("expected a pin more specific than the installed version to not match")
	}
}

func TestFindPinnedNodeInstall_ResolvesViaMiseLayout(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MISE_DATA_DIR", root)
	t.Setenv("NVM_DIR", filepath.Join(root, "no-nvm-here"))
	t.Setenv("VOLTA_HOME", filepath.Join(root, "no-volta-here"))
	t.Setenv("FNM_DIR", filepath.Join(root, "no-fnm-here"))
	t.Setenv("ASDF_DATA_DIR", filepath.Join(root, "no-asdf-here"))

	installBin := filepath.Join(root, "installs", "node", "20.19.0", "bin")
	if err := os.MkdirAll(installBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(installBin, nodeBinaryName()))

	// A newer, non-matching install must not be picked over the exact match.
	otherBin := filepath.Join(root, "installs", "node", "22.0.0", "bin")
	if err := os.MkdirAll(otherBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(otherBin, nodeBinaryName()))

	binDir, matched, manager, _, err := findPinnedNodeInstall(nodeVersionTestContext(root), []int{20, 19, 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if binDir != installBin {
		t.Fatalf("binDir = %q, want %q", binDir, installBin)
	}
	if matched != "20.19.0" || manager != "mise" {
		t.Fatalf("matched=%q manager=%q, want 20.19.0/mise", matched, manager)
	}
}

func TestFindPinnedNodeInstall_WindowsManagerLayouts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MISE_DATA_DIR", filepath.Join(root, "mise"))
	t.Setenv("NVM_HOME", filepath.Join(root, "nvm"))
	t.Setenv("NVM_DIR", filepath.Join(root, "ignored-nvm-dir"))
	t.Setenv("VOLTA_HOME", filepath.Join(root, "volta"))
	t.Setenv("FNM_DIR", filepath.Join(root, "fnm"))
	t.Setenv("ASDF_DATA_DIR", filepath.Join(root, "asdf"))

	cases := []struct {
		manager string
		binDir  string
	}{
		{"mise", filepath.Join(root, "mise", "installs", "node", "20.19.0")},
		{"nvm", filepath.Join(root, "nvm", "v20.19.0")},
		{"volta", filepath.Join(root, "volta", "tools", "image", "node", "20.19.0")},
		{"fnm", filepath.Join(root, "fnm", "node-versions", "v20.19.0", "installation")},
		{"asdf", filepath.Join(root, "asdf", "installs", "nodejs", "20.19.0", "bin")},
	}
	for _, tc := range cases {
		t.Run(tc.manager, func(t *testing.T) {
			for _, other := range cases {
				if err := os.RemoveAll(other.binDir); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(tc.binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tc.binDir, "node.exe"), []byte("node"), 0o644); err != nil {
				t.Fatal(err)
			}

			binDir, matched, manager, _, err := findPinnedNodeInstallForOS([]int{20, 19, 0}, "windows", nodeVersionManagersForOS("windows"), func(string) ([]int, bool) {
				return []int{20, 19, 0}, true
			})
			if err != nil {
				t.Fatalf("resolve Windows %s layout: %v", tc.manager, err)
			}
			if binDir != tc.binDir || matched != "20.19.0" || manager != tc.manager {
				t.Fatalf("got bin=%q version=%q manager=%q, want %q, 20.19.0, %q", binDir, matched, manager, tc.binDir, tc.manager)
			}
		})
	}
}

func TestNodeVersionManagers_DefaultRootsAreOSSpecific(t *testing.T) {
	home := filepath.Join("root", "home")
	noEnv := func(string) string { return "" }
	cases := []struct {
		goos string
		want map[string]string
	}{
		{
			goos: "linux",
			want: map[string]string{
				"mise":  filepath.Join(home, ".local", "share", "mise", "installs", "node"),
				"nvm":   filepath.Join(home, ".nvm", "versions", "node"),
				"volta": filepath.Join(home, ".volta", "tools", "image", "node"),
				"fnm":   filepath.Join(home, ".local", "share", "fnm", "node-versions"),
				"asdf":  filepath.Join(home, ".asdf", "installs", "nodejs"),
			},
		},
		{
			goos: "darwin",
			want: map[string]string{
				"fnm": filepath.Join(home, "Library", "Application Support", "fnm", "node-versions"),
			},
		},
		{
			goos: "windows",
			want: map[string]string{
				"mise":  filepath.Join(home, "AppData", "Local", "mise", "installs", "node"),
				"nvm":   filepath.Join(home, "AppData", "Roaming", "nvm"),
				"volta": filepath.Join(home, "AppData", "Local", "Volta", "tools", "image", "node"),
				"fnm":   filepath.Join(home, "AppData", "Roaming", "fnm", "node-versions"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			for _, manager := range nodeVersionManagersForOSWithHome(tc.goos, home, noEnv) {
				want, asserted := tc.want[manager.name]
				if !asserted {
					continue
				}
				got, ok := manager.installsDir()
				if !ok || got != want {
					t.Fatalf("%s default root = %q, %v; want %q, true", manager.name, got, ok, want)
				}
			}
		})
	}
}

func TestFindPinnedNodeInstall_PicksHighestMatchingForPartialPin(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MISE_DATA_DIR", root)
	t.Setenv("NVM_DIR", filepath.Join(root, "no-nvm-here"))
	t.Setenv("VOLTA_HOME", filepath.Join(root, "no-volta-here"))
	t.Setenv("FNM_DIR", filepath.Join(root, "no-fnm-here"))
	t.Setenv("ASDF_DATA_DIR", filepath.Join(root, "no-asdf-here"))

	for _, version := range []string{"20.10.0", "20.19.0"} {
		bin := filepath.Join(root, "installs", "node", version, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		writeNodeVersion(t, filepath.Join(bin, nodeBinaryName()), version)
	}

	binDir, matched, _, _, err := findPinnedNodeInstall(nodeVersionTestContext(root), []int{20})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(root, "installs", "node", "20.19.0", "bin")
	if binDir != want {
		t.Fatalf("binDir = %q, want %q", binDir, want)
	}
	if matched != "20.19.0" {
		t.Fatalf("matched = %q, want 20.19.0", matched)
	}
}

func TestFindPinnedNodeInstall_NoneFoundFailsWithCheckedList(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MISE_DATA_DIR", filepath.Join(root, "mise"))
	t.Setenv("NVM_DIR", filepath.Join(root, "nvm"))
	t.Setenv("VOLTA_HOME", filepath.Join(root, "volta"))
	t.Setenv("FNM_DIR", filepath.Join(root, "fnm"))
	t.Setenv("ASDF_DATA_DIR", filepath.Join(root, "asdf"))

	_, _, _, checked, err := findPinnedNodeInstall(nodeVersionTestContext(root), []int{20, 19, 0})
	if err == nil {
		t.Fatal("expected an error when no manager has a matching install")
	}
	if len(checked) == 0 {
		t.Fatal("expected the checked manager list to be non-empty for a clear error message")
	}
}

func TestNodeVersionOverride_NoPinIsNoop(t *testing.T) {
	dir := t.TempDir()
	env, note, err := nodeVersionOverride(nodeVersionTestContext(dir))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env != nil || note != "" {
		t.Fatalf("expected no-op override for an unpinned repository, got env=%v note=%q", env, note)
	}
}

func TestNodeVersionOverride_UnresolvableRangeFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".nvmrc"), "lts/*\n")

	_, _, err := nodeVersionOverride(nodeVersionTestContext(dir))
	if err == nil {
		t.Fatal("expected an error for an alias/range pin no-mistakes cannot resolve")
	}
}

func TestNodeVersionOverride_NoMatchingInstallFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".nvmrc"), "20.19.0\n")

	root := t.TempDir()
	t.Setenv("MISE_DATA_DIR", filepath.Join(root, "mise"))
	t.Setenv("NVM_DIR", filepath.Join(root, "nvm"))
	t.Setenv("VOLTA_HOME", filepath.Join(root, "volta"))
	t.Setenv("FNM_DIR", filepath.Join(root, "fnm"))
	t.Setenv("ASDF_DATA_DIR", filepath.Join(root, "asdf"))
	t.Setenv("PATH", root) // no node on PATH either

	_, _, err := nodeVersionOverride(nodeVersionTestContext(dir))
	if err == nil {
		t.Fatal("expected an error when the pinned version cannot be found anywhere on the host")
	}
}

func TestNodeVersionOverride_ResolvesAndPrependsBinDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".nvmrc"), "20.19.0\n")

	root := t.TempDir()
	t.Setenv("MISE_DATA_DIR", root)
	t.Setenv("NVM_DIR", filepath.Join(root, "no-nvm-here"))
	t.Setenv("VOLTA_HOME", filepath.Join(root, "no-volta-here"))
	t.Setenv("FNM_DIR", filepath.Join(root, "no-fnm-here"))
	t.Setenv("ASDF_DATA_DIR", filepath.Join(root, "no-asdf-here"))
	t.Setenv("PATH", root) // ensure no unrelated "node" shadows the pinned install

	installBin := filepath.Join(root, "installs", "node", "20.19.0", "bin")
	if err := os.MkdirAll(installBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(installBin, nodeBinaryName()))

	env, note, err := nodeVersionOverride(nodeVersionTestContext(dir))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if note == "" {
		t.Fatal("expected a note describing which pinned Node was selected")
	}
	if len(env) != 1 {
		t.Fatalf("expected exactly one env override entry, got %v", env)
	}
	wantPrefix := "PATH=" + installBin + string(os.PathListSeparator)
	if env[0][:len(wantPrefix)] != wantPrefix {
		t.Fatalf("env[0] = %q, want prefix %q", env[0], wantPrefix)
	}
}

func TestFindPinnedNodeInstall_RejectsNonExecutableCandidate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable mode test")
	}
	root := t.TempDir()
	t.Setenv("MISE_DATA_DIR", root)
	t.Setenv("NVM_DIR", filepath.Join(root, "nvm"))
	t.Setenv("VOLTA_HOME", filepath.Join(root, "volta"))
	t.Setenv("FNM_DIR", filepath.Join(root, "fnm"))
	t.Setenv("ASDF_DATA_DIR", filepath.Join(root, "asdf"))
	bin := filepath.Join(root, "installs", "node", "20.19.0", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, nodeBinaryName()), []byte("node"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := findPinnedNodeInstall(nodeVersionTestContext(root), []int{20, 19, 0}); err == nil {
		t.Fatal("expected a non-executable Node candidate to be rejected")
	}
}

func TestNodeVersionOverride_ProvesExactHostNode(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".nvmrc"), "20.19.0\n")
	hostBin := t.TempDir()
	writeExecutable(t, filepath.Join(hostBin, nodeBinaryName()))
	t.Setenv("PATH", hostBin)

	env, note, err := nodeVersionOverride(nodeVersionTestContext(dir))
	if err != nil {
		t.Fatal(err)
	}
	if env != nil {
		t.Fatalf("exact proven host Node should need no override, got %v", env)
	}
	if !strings.Contains(note, `"20.19.0" from .nvmrc to exact v20.19.0`) || !strings.Contains(note, hostBin) {
		t.Fatalf("resolution note does not state declaration, source, exact version, and executable: %q", note)
	}
}

func TestNodeVersionOverride_PartialPinUsesHighestManagedInstallAndLogsResolution(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".nvmrc"), "20\n")
	root := t.TempDir()
	t.Setenv("MISE_DATA_DIR", root)
	t.Setenv("NVM_DIR", filepath.Join(root, "nvm"))
	t.Setenv("VOLTA_HOME", filepath.Join(root, "volta"))
	t.Setenv("FNM_DIR", filepath.Join(root, "fnm"))
	t.Setenv("ASDF_DATA_DIR", filepath.Join(root, "asdf"))
	t.Setenv("PATH", t.TempDir())
	for _, version := range []string{"20.10.0", "20.19.0"} {
		bin := filepath.Join(root, "installs", "node", version, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		writeNodeVersion(t, filepath.Join(bin, nodeBinaryName()), version)
	}

	env, note, err := nodeVersionOverride(nodeVersionTestContext(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || !strings.HasPrefix(env[0], "PATH="+filepath.Join(root, "installs", "node", "20.19.0", "bin")) {
		t.Fatalf("partial pin override = %v", env)
	}
	if !strings.Contains(note, `"20" from .nvmrc to exact v20.19.0`) {
		t.Fatalf("partial resolution is not visible in note: %q", note)
	}
}

func TestFindPinnedNodeInstall_PartialDirectoryCanSatisfyFullPinAfterProbe(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "20", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	nodePath := filepath.Join(bin, nodeBinaryName())
	writeExecutable(t, nodePath)
	manager := nodeVersionManager{
		name: "test",
		installsDir: func() (string, bool) {
			return root, true
		},
		binSubpath: []string{"bin"},
	}

	gotBin, matched, _, _, err := findPinnedNodeInstallForOS([]int{20, 19, 0}, runtime.GOOS, []nodeVersionManager{manager}, func(path string) ([]int, bool) {
		if path != nodePath {
			t.Fatalf("probed %q, want %q", path, nodePath)
		}
		return []int{20, 19, 0}, true
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBin != bin || matched != "20.19.0" {
		t.Fatalf("verified partial-directory candidate = %q, %q; want %q, 20.19.0", gotBin, matched, bin)
	}
}

func TestFindPinnedNodeInstall_PartialDirectoryRequiresVerifiedExactVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	root := t.TempDir()
	t.Setenv("MISE_DATA_DIR", root)
	t.Setenv("NVM_DIR", filepath.Join(root, "nvm"))
	t.Setenv("VOLTA_HOME", filepath.Join(root, "volta"))
	t.Setenv("FNM_DIR", filepath.Join(root, "fnm"))
	t.Setenv("ASDF_DATA_DIR", filepath.Join(root, "asdf"))
	bin := filepath.Join(root, "installs", "node", "20", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	nodePath := filepath.Join(bin, nodeBinaryName())
	writeNodeVersion(t, nodePath, "20.19.7")

	gotBin, matched, _, _, err := findPinnedNodeInstall(nodeVersionTestContext(root), []int{20})
	if err != nil {
		t.Fatal(err)
	}
	if gotBin != bin || matched != "20.19.7" {
		t.Fatalf("verified partial-directory candidate = %q, %q; want %q, 20.19.7", gotBin, matched, bin)
	}

	writeNodeVersion(t, nodePath, "21.0.0")
	if _, _, _, _, err := findPinnedNodeInstall(nodeVersionTestContext(root), []int{20}); err == nil {
		t.Fatal("expected candidate whose executed version does not match to be rejected")
	}
}

func TestPathNodeVersion_ResolvesRelativePATHFromWorkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	workDir := t.TempDir()
	bin := filepath.Join(workDir, "relative-bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	nodePath := filepath.Join(bin, "node")
	writeExecutable(t, nodePath)
	t.Setenv("PATH", "relative-bin")

	parts, gotPath, ok := pathNodeVersion(nodeVersionTestContext(workDir))
	if !ok || !intSlicesEqual(parts, []int{20, 19, 0}) {
		t.Fatalf("relative worktree Node version = %v, %v; want 20.19.0, true", parts, ok)
	}
	if gotPath != nodePath {
		t.Fatalf("resolved Node = %q, want %q", gotPath, nodePath)
	}
}

func TestPathNodeVersion_UsesStepCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "node"), []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sctx := &pipeline.StepContext{Ctx: ctx, WorkDir: t.TempDir(), Env: []string{"PATH=" + bin}}
	if _, _, ok := pathNodeVersion(sctx); ok {
		t.Fatal("expected cancelled supervised probe to fail")
	}
}

func nodeVersionTestContext(workDir string) *pipeline.StepContext {
	return &pipeline.StepContext{Ctx: context.Background(), WorkDir: workDir}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	writeNodeVersion(t, path, "20.19.0")
}

func writeNodeVersion(t *testing.T, path, version string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho v"+version+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
