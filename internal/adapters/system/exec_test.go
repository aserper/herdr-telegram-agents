package system

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExecRunnerCapturesBothStreams(t *testing.T) {
	r := NewExecRunner(nil)
	stdout, stderr, err := r.Run(context.Background(),
		[]string{"sh", "-c", "echo out; echo err >&2"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(string(stdout)) != "out" || strings.TrimSpace(string(stderr)) != "err" {
		t.Fatalf("stdout = %q, stderr = %q", stdout, stderr)
	}
}

func TestExecRunnerFailsWithStderr(t *testing.T) {
	r := NewExecRunner(nil)
	_, _, err := r.Run(context.Background(), []string{"sh", "-c", "echo boom >&2; exit 3"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want stderr in the message", err)
	}
}

func TestExecRunnerHonoursContextDeadline(t *testing.T) {
	r := NewExecRunner(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := r.Run(ctx, []string{"sleep", "5"})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}
