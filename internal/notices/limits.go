package notices

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

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
	return RunCommandLimit(cmd, label, MaxResponseBytes)
}

func RunCommandLimit(cmd *exec.Cmd, label string, responseLimit int) ([]byte, error) {
	if responseLimit <= 0 || responseLimit > MaxResponseBytes {
		return nil, fmt.Errorf("%s: %w: invalid remaining response budget %d", label, ErrCapacity, responseLimit)
	}
	stdout := NewCapture(responseLimit)
	stderr := NewCapture(MaxDiagnosticBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := shellenv.RunShellCommand(cmd)
	if stdout.Overflowed() {
		return nil, fmt.Errorf("%s: %w: response exceeds remaining %d-byte budget", label, ErrCapacity, responseLimit)
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

func SplitIncludedResponse(data []byte) (map[string]string, []byte, error) {
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	parts := strings.SplitN(normalized, "\n\n", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return nil, nil, errors.New("response is missing included HTTP headers")
	}
	headers := make(map[string]string)
	lines := strings.Split(parts[0], "\n")
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, nil, fmt.Errorf("malformed response header %q", line)
		}
		headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
	return headers, []byte(parts[1]), nil
}

func ValidateAggregate(pages, comments, responseBytes int, hasContinuation bool) error {
	if err := ValidateCounts(pages, comments); err != nil {
		return err
	}
	if responseBytes > MaxResponseBytes {
		return fmt.Errorf("%w: aggregate responses exceed %d bytes", ErrCapacity, MaxResponseBytes)
	}
	if hasContinuation && (pages == MaxPages || comments == MaxComments || responseBytes == MaxResponseBytes) {
		return fmt.Errorf("%w: unread continuation exceeds notice read limits", ErrCapacity)
	}
	return nil
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
