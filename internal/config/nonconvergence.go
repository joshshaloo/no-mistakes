package config

import (
	"log/slog"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/convergence"
)

// NonconvergenceRaw is global-only: repository input must not select an external
// signal endpoint, enable transmission, or name the operator's secret file.
type NonconvergenceRaw struct {
	Enabled *bool   `yaml:"enabled"`
	Model   *string `yaml:"model"`
	BaseURL *string `yaml:"base_url"`
	KeyFile *string `yaml:"key_file"`
	Timeout *string `yaml:"timeout"`
}

// Nonconvergence is an optional signal, never a validation gate.
type Nonconvergence struct {
	Enabled bool
	Model   string
	BaseURL string
	KeyFile string
	Timeout time.Duration
}

func validateNonconvergenceRaw(raw *NonconvergenceRaw) error {
	if raw.Timeout == nil || strings.TrimSpace(*raw.Timeout) == "" {
		return nil
	}
	timeout, err := parsePositiveDuration("nonconvergence.timeout", *raw.Timeout)
	if err != nil {
		return err
	}
	if timeout <= convergence.MaximumTimeout {
		return nil
	}
	slog.Info("invalid nonconvergence.timeout; signal disabled", "configured_timeout", timeout, "maximum_timeout", convergence.MaximumTimeout)
	disabled := false
	raw.Enabled = &disabled
	raw.Timeout = nil
	return nil
}

func resolvedNonconvergence(raw NonconvergenceRaw) Nonconvergence {
	out := Nonconvergence{Model: convergence.DefaultModel, BaseURL: convergence.DefaultBaseURL, Timeout: convergence.DefaultTimeout}
	if raw.Enabled != nil {
		out.Enabled = *raw.Enabled
	}
	if raw.Model != nil && strings.TrimSpace(*raw.Model) != "" {
		out.Model = strings.TrimSpace(*raw.Model)
	}
	if raw.BaseURL != nil && strings.TrimSpace(*raw.BaseURL) != "" {
		out.BaseURL = strings.TrimSpace(*raw.BaseURL)
	}
	if raw.KeyFile != nil && strings.TrimSpace(*raw.KeyFile) != "" {
		out.KeyFile = strings.TrimSpace(*raw.KeyFile)
	}
	if raw.Timeout != nil && strings.TrimSpace(*raw.Timeout) != "" {
		if d, err := parsePositiveDuration("nonconvergence.timeout", *raw.Timeout); err == nil {
			if d > convergence.MaximumTimeout {
				out.Enabled = false
			} else {
				out.Timeout = d
			}
		}
	}
	return out
}

// ConvergenceSettings maps only the resolved host-owned configuration.
func (c *Config) ConvergenceSettings() convergence.Settings {
	if c == nil {
		return convergence.Settings{}
	}
	return convergence.Settings{Enabled: c.Nonconvergence.Enabled, Model: c.Nonconvergence.Model, BaseURL: c.Nonconvergence.BaseURL, KeyFile: c.Nonconvergence.KeyFile, Timeout: c.Nonconvergence.Timeout}
}
