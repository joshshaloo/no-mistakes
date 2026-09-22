package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/convergence"
)

func TestNonconvergenceDefaultsToOff(t *testing.T) {
	cfg := Merge(DefaultGlobalConfig(), &RepoConfig{})

	if cfg.Nonconvergence.Enabled {
		t.Fatal("the non-convergence signal must be off by default: a host that configured nothing gets today's behavior")
	}
	if cfg.Nonconvergence.Model != convergence.DefaultModel {
		t.Errorf("model = %q, want the pinned default %q", cfg.Nonconvergence.Model, convergence.DefaultModel)
	}
	if cfg.Nonconvergence.BaseURL != convergence.DefaultBaseURL {
		t.Errorf("base_url = %q, want %q", cfg.Nonconvergence.BaseURL, convergence.DefaultBaseURL)
	}
	if cfg.Nonconvergence.Timeout != convergence.DefaultTimeout {
		t.Errorf("timeout = %v, want %v", cfg.Nonconvergence.Timeout, convergence.DefaultTimeout)
	}
	if cfg.Nonconvergence.KeyFile != "" {
		t.Errorf("key_file = %q, want empty: no secret location is invented", cfg.Nonconvergence.KeyFile)
	}
}

func TestNonconvergenceGlobalOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	contents := "nonconvergence:\n  enabled: true\n  model: jev-1.14\n  base_url: https://example.test/api\n  key_file: /home/op/secrets.env\n  timeout: 4s\n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	global, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("load global: %v", err)
	}
	cfg := Merge(global, &RepoConfig{})

	if !cfg.Nonconvergence.Enabled {
		t.Error("enabled override was not applied")
	}
	if cfg.Nonconvergence.Model != "jev-1.14" {
		t.Errorf("model = %q", cfg.Nonconvergence.Model)
	}
	if cfg.Nonconvergence.BaseURL != "https://example.test/api" {
		t.Errorf("base_url = %q", cfg.Nonconvergence.BaseURL)
	}
	if cfg.Nonconvergence.KeyFile != "/home/op/secrets.env" {
		t.Errorf("key_file = %q", cfg.Nonconvergence.KeyFile)
	}
	if cfg.Nonconvergence.Timeout != 4*time.Second {
		t.Errorf("timeout = %v", cfg.Nonconvergence.Timeout)
	}
}

// The detector sends run state to a third-party service and names the file a
// secret is read from, so a pushed branch must never be able to turn it on or
// aim it somewhere else.
func TestNonconvergenceCannotBeEnabledByARepoConfig(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte("nonconvergence:\n  enabled: true\n  key_file: /tmp/attacker.env\n  base_url: https://attacker.test/api\n"))
	if err != nil {
		t.Fatalf("a repo config naming nonconvergence must parse, not fail: %v", err)
	}

	cfg := Merge(DefaultGlobalConfig(), repo)
	if cfg.Nonconvergence.Enabled {
		t.Fatal("a repo config must not be able to enable the non-convergence signal")
	}
	if cfg.Nonconvergence.KeyFile != "" {
		t.Fatalf("a repo config must not be able to name the key file, got %q", cfg.Nonconvergence.KeyFile)
	}
	if cfg.Nonconvergence.BaseURL != convergence.DefaultBaseURL {
		t.Fatalf("a repo config must not be able to redirect where state is sent, got %q", cfg.Nonconvergence.BaseURL)
	}
}

func TestNonconvergenceRejectsAnUnparseableTimeoutAtLoadTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("nonconvergence:\n  enabled: true\n  timeout: not-a-duration\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(path); err == nil {
		t.Fatal("an unparseable timeout must fail at load time rather than silently falling back")
	}
}

func TestNonconvergenceTimeoutAboveMaximumDisablesSignalWithoutFailingLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	configured := convergence.MaximumTimeout + time.Second
	contents := "nonconvergence:\n  enabled: true\n  timeout: " + configured.String() + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	global, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("an advisory detector timeout must not fail global config loading: %v", err)
	}
	cfg := Merge(global, &RepoConfig{})
	settings := cfg.ConvergenceSettings()
	if settings.Enabled {
		t.Fatal("an above-maximum timeout must disable the detector")
	}
	t.Setenv("OPENROUTER_API_KEY", "configured-for-test")
	if detector := convergence.New(settings); detector != nil {
		t.Fatal("disabled settings must not produce a live detector")
	}
	for _, want := range []string{configured.String(), convergence.MaximumTimeout.String()} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("rejection log %q does not name %q", logs.String(), want)
		}
	}
}

func TestNonconvergenceTimeoutAtMaximumIsAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "nonconvergence:\n  enabled: true\n  timeout: " + convergence.MaximumTimeout.String() + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	global, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("load global: %v", err)
	}
	cfg := Merge(global, &RepoConfig{})
	if !cfg.Nonconvergence.Enabled {
		t.Fatal("the maximum timeout must remain enabled")
	}
	if cfg.Nonconvergence.Timeout != convergence.MaximumTimeout {
		t.Fatalf("timeout = %v, want %v", cfg.Nonconvergence.Timeout, convergence.MaximumTimeout)
	}
}

func TestConvergenceSettingsCarryTheResolvedConfig(t *testing.T) {
	cfg := &Config{Nonconvergence: Nonconvergence{
		Enabled: true, Model: "m", BaseURL: "https://b.test", KeyFile: "/k.env", Timeout: 3 * time.Second,
	}}
	got := cfg.ConvergenceSettings()
	if !got.Enabled || got.Model != "m" || got.BaseURL != "https://b.test" || got.KeyFile != "/k.env" || got.Timeout != 3*time.Second {
		t.Fatalf("settings did not carry the resolved config: %+v", got)
	}
	var nilCfg *Config
	if nilCfg.ConvergenceSettings().Enabled {
		t.Fatal("a nil config must produce disabled settings")
	}
}

// The default config file is the documentation an operator reads first, so it
// must describe the feature as a signal and never imply it gates anything.
func TestDefaultConfigYAMLDocumentsNonconvergenceAsAnOptionalSignal(t *testing.T) {
	if !strings.Contains(defaultConfigYAML, "nonconvergence:") {
		t.Fatal("default config must mention the nonconvergence block")
	}
	if !strings.Contains(defaultConfigYAML, "# nonconvergence:") {
		t.Fatal("the nonconvergence block must be commented out: the feature is off by default")
	}
	for _, phrase := range []string{"SIGNAL, never a gate", "key_file"} {
		if !strings.Contains(defaultConfigYAML, phrase) {
			t.Errorf("default config must state %q", phrase)
		}
	}
}
