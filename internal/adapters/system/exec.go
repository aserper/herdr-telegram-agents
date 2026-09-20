package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

const (
	// execWaitDelay bounds how long a run waits for its pipes once the
	// context killed the command: a child that inherited stdout would
	// otherwise keep Wait blocked until it exits on its own.
	execWaitDelay = 2 * time.Second
	// execMaxOutput is the most one stream may produce; the rest is cut so
	// a runaway command cannot grow the daemon's memory.
	execMaxOutput = 8 << 20
)

// execMaxOutputErr stops the stream copy once execMaxOutput is reached.
var execMaxOutputErr = errors.New("output capped")

// ExecRunner runs one command by argv and returns both streams. It is the
// process primitive behind adapters that must spawn a client (the remote
// prompt decorator runs ssh); the timeout is the caller's context.
type ExecRunner struct {
	log *slog.Logger
}

// NewExecRunner returns a runner that logs one debug line per call.
func NewExecRunner(log *slog.Logger) *ExecRunner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ExecRunner{log: log}
}

// Run executes argv[0] with the rest as arguments. stdout and stderr come
// back even when the command failed, so a caller can parse what was printed
// before the failure. A non-zero exit is an error carrying the trimmed
// stderr (stdout when stderr is empty).
func (r *ExecRunner) Run(ctx context.Context, argv []string) (stdout, stderr []byte, err error) {
	if len(argv) == 0 {
		return nil, nil, errors.New("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = execWaitDelay
	out := &cappedBuffer{max: execMaxOutput}
	errOut := &cappedBuffer{max: execMaxOutput}
	cmd.Stdout, cmd.Stderr = out, errOut
	start := time.Now()
	runErr := cmd.Run()
	stdout, stderr = out.Bytes(), errOut.Bytes()
	r.log.Debug("exec run", slog.String("argv0", argv[0]), slog.Int("args", len(argv)-1),
		slog.Int64("dur_ms", time.Since(start).Milliseconds()), slog.Int("stdout_bytes", len(stdout)),
		slog.Int("stderr_bytes", len(stderr)), slog.Any("err", runErr))
	switch {
	case runErr == nil:
		return stdout, stderr, nil
	case ctx.Err() != nil:
		return stdout, stderr, fmt.Errorf("%s: %w", argv[0], ctx.Err())
	}
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(stdout))
	}
	if msg != "" {
		return stdout, stderr, fmt.Errorf("%s: %w: %s", argv[0], runErr, firstLine(msg))
	}
	return stdout, stderr, fmt.Errorf("%s: %w", argv[0], runErr)
}

// cappedBuffer collects up to max bytes and refuses the rest, which stops
// exec's copy and lets the process die on a closed pipe.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := b.max - b.buf.Len()
	if room <= 0 {
		return 0, execMaxOutputErr
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		return room, execMaxOutputErr
	}
	return b.buf.Write(p)
}

// Bytes returns what was collected.
func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }
