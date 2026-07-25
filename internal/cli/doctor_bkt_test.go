package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

func TestDoctorVerifiesAuthenticatedBKTCloudKeychainContext(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte("agent: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "claude")
	writeDoctorBKTBinary(t, binDir)
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	line := "bkt             v0.30.0+ authenticated with Bitbucket Cloud Keychain context"
	if !strings.Contains(out, line) {
		t.Fatalf("doctor output missing %q:\n%s", line, out)
	}
}

func TestDoctorReportsDirectBitbucketCredentialPrecedenceWithoutToken(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("NO_MISTAKES_BITBUCKET_EMAIL", "direct@example.com")
	t.Setenv("NO_MISTAKES_BITBUCKET_API_TOKEN", "ATBB_doctor-secret-fixture")
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte("agent: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bitbucket       direct REST credentials configured") {
		t.Fatalf("doctor did not report direct Bitbucket precedence:\n%s", out)
	}
	if strings.Contains(out, "ATBB_doctor-secret-fixture") {
		t.Fatalf("doctor leaked Bitbucket token:\n%s", out)
	}
}

func writeDoctorBKTBinary(t *testing.T, dir string) {
	t.Helper()
	name := "bkt"
	contents := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo 'bkt version 0.30.0'
  exit 0
fi
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then
  echo '{"active_context":"cloud","hosts":[{"key":"api.bitbucket.org","kind":"cloud","base_url":"https://api.bitbucket.org/2.0","username":"dev@example.com","token_source":"keyring"}],"contexts":[{"name":"cloud","host":"api.bitbucket.org","workspace":"team","active":true}]}'
  exit 0
fi
exit 1
`
	if runtime.GOOS == "windows" {
		name += ".cmd"
		contents = `@echo off
if "%1"=="--version" (
  echo bkt version 0.30.0
  exit /b 0
)
if "%1"=="auth" (
  echo {"active_context":"cloud","hosts":[{"key":"api.bitbucket.org","kind":"cloud","base_url":"https://api.bitbucket.org/2.0","username":"dev@example.com","token_source":"keyring"}],"contexts":[{"name":"cloud","host":"api.bitbucket.org","workspace":"team","active":true}]}
  exit /b 0
)
exit /b 1
`
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake bkt: %v", err)
	}
}
