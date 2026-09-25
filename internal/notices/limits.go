package notices

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const (
	MaxResponseBytes   = 2 * 1024 * 1024
	MaxDiagnosticBytes = 32 * 1024
	MaxComments        = 5000
	MaxPages           = 100
)

var ErrCapacity = errors.New("PR notice read capacity exceeded")

type Capture struct {
	limit    int
	data     []byte
	overflow bool
}

func NewCapture(limit int) *Capture { return &Capture{limit: limit} }

func (c *Capture) Write(p []byte) (int, error) {
	if len(c.data)+len(p) > c.limit {
		remaining := c.limit - len(c.data)
		if remaining > 0 {
			c.data = append(c.data, p[:remaining]...)
		}
		c.overflow = true
		return len(p), ErrCapacity
	}
	c.data = append(c.data, p...)
	return len(p), nil
}

func (c *Capture) Bytes() []byte    { return append([]byte(nil), c.data...) }
func (c *Capture) Overflowed() bool { return c.overflow }

func RunCommand(cmd *exec.Cmd, label string) ([]byte, error) {
	stdout := NewCapture(MaxResponseBytes)
	stderr := NewCapture(MaxDiagnosticBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := shellenv.RunShellCommand(cmd)
	if stdout.Overflowed() {
		return nil, fmt.Errorf("%s: %w: response exceeds %d bytes", label, ErrCapacity, MaxResponseBytes)
	}
	if stderr.Overflowed() {
		return nil, fmt.Errorf("%s: %w: diagnostic output exceeds %d bytes", label, ErrCapacity, MaxDiagnosticBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", label, string(bytes.TrimSpace(stderr.Bytes())), err)
	}
	return stdout.Bytes(), nil
}

func ReadAll(r io.Reader, label string) ([]byte, error) {
	return ReadAllLimit(r, label, MaxResponseBytes)
}

func ReadAllLimit(r io.Reader, label string, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%s: %w: response exceeds %d bytes", label, ErrCapacity, limit)
	}
	return data, nil
}

func ValidateCounts(pages, comments int) error {
	if pages > MaxPages {
		return fmt.Errorf("%w: more than %d pages", ErrCapacity, MaxPages)
	}
	if comments > MaxComments {
		return fmt.Errorf("%w: more than %d comments", ErrCapacity, MaxComments)
	}
	return nil
}
