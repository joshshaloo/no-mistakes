package notices

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCaptureAcceptsLimitAndRefusesOverflow(t *testing.T) {
	capture := NewCapture(4)
	if _, err := capture.Write([]byte("1234")); err != nil {
		t.Fatalf("at-limit write: %v", err)
	}
	if capture.Overflowed() {
		t.Fatal("at-limit write marked overflow")
	}
	if _, err := capture.Write([]byte("5")); !errors.Is(err, ErrCapacity) {
		t.Fatalf("over-limit error = %v, want capacity error", err)
	}
	if got := string(capture.Bytes()); got != "1234" {
		t.Fatalf("bounded bytes = %q", got)
	}
}

func TestNoticeHelper(t *testing.T) {
	if os.Getenv("NOTICE_HELPER") == "output" {
		_, _ = os.Stdout.Write(make([]byte, MaxResponseBytes+1))
		os.Exit(0)
	}
	if os.Getenv("NOTICE_HELPER") == "sleep" {
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
}

func TestRunCommandBoundsOutputAndHonorsCancellation(t *testing.T) {
	t.Run("large response", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=TestNoticeHelper")
		cmd.Env = append(os.Environ(), "NOTICE_HELPER=output")
		_, err := RunCommand(cmd, "read test notices")
		if !errors.Is(err, ErrCapacity) {
			t.Fatalf("error = %v, want capacity error", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestNoticeHelper")
		cmd.Env = append(os.Environ(), "NOTICE_HELPER=sleep")
		_, err := RunCommand(cmd, "read test notices")
		if err == nil {
			t.Fatal("cancelled command succeeded")
		}
	})
}

func TestValidateCountsBoundsPagesAndComments(t *testing.T) {
	if err := ValidateCounts(MaxPages, MaxComments); err != nil {
		t.Fatalf("at-limit counts: %v", err)
	}
	for name, counts := range map[string][2]int{
		"pages":    {MaxPages + 1, 0},
		"comments": {1, MaxComments + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCounts(counts[0], counts[1]); !errors.Is(err, ErrCapacity) || !strings.Contains(err.Error(), name) {
				t.Fatalf("error = %v, want named capacity error", err)
			}
		})
	}
}
