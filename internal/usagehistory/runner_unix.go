//go:build darwin || linux

package usagehistory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
)

type commandSpec struct {
	name      string
	args      []string
	dir       string
	env       []string
	maxOutput int64
}

type commandRunner interface {
	Run(context.Context, commandSpec) ([]byte, error)
}

type processRunner struct{}

type processErrorKind uint8

const (
	processErrorStart processErrorKind = iota
	processErrorExit
	processErrorCanceled
	processErrorOutputLimit
)

type processError struct {
	kind       processErrorKind
	safeDetail string
	cause      error
}

func (e *processError) Error() string { return e.safeDetail }
func (e *processError) Unwrap() error { return e.cause }

type boundedOutput struct {
	mu        sync.Mutex
	stdout    bytes.Buffer
	remaining int64
	exceeded  chan struct{}
	once      sync.Once
}

type boundedWriter struct {
	output *boundedOutput
	stdout bool
}

func newBoundedOutput(max int64) *boundedOutput {
	return &boundedOutput{remaining: max, exceeded: make(chan struct{})}
}

func (w boundedWriter) Write(p []byte) (int, error) {
	w.output.mu.Lock()
	originalRemaining := w.output.remaining
	if w.output.remaining > 0 {
		n := int64(len(p))
		if n > w.output.remaining {
			n = w.output.remaining
		}
		if w.stdout {
			_, _ = w.output.stdout.Write(p[:n])
		}
		w.output.remaining -= n
	}
	exceeded := int64(len(p)) > originalRemaining
	w.output.mu.Unlock()
	if exceeded {
		w.output.once.Do(func() { close(w.output.exceeded) })
	}
	return len(p), nil
}

func (o *boundedOutput) bytes() []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	return bytes.Clone(o.stdout.Bytes())
}

func (processRunner) Run(ctx context.Context, spec commandSpec) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, &processError{kind: processErrorCanceled, safeDetail: "tool was canceled", cause: context.Cause(ctx)}
	}
	cmd := exec.Command(spec.name, spec.args...)
	cmd.Dir = spec.dir
	cmd.Env = spec.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	output := newBoundedOutput(spec.maxOutput)
	cmd.Stdout = boundedWriter{output: output, stdout: true}
	cmd.Stderr = boundedWriter{output: output}
	if err := cmd.Start(); err != nil {
		return nil, &processError{kind: processErrorStart, safeDetail: "could not start tool", cause: err}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if output.wasExceeded() {
			return nil, &processError{kind: processErrorOutputLimit, safeDetail: "tool output exceeded the configured limit"}
		}
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return nil, &processError{kind: processErrorExit, safeDetail: fmt.Sprintf("tool exited with status %d", exitErr.ExitCode())}
			}
			return nil, &processError{kind: processErrorExit, safeDetail: "tool execution failed", cause: err}
		}
		return output.bytes(), nil
	case <-ctx.Done():
		killProcessGroup(cmd.Process.Pid)
		<-done
		return nil, &processError{kind: processErrorCanceled, safeDetail: "tool was canceled", cause: context.Cause(ctx)}
	case <-output.exceeded:
		killProcessGroup(cmd.Process.Pid)
		<-done
		return nil, &processError{kind: processErrorOutputLimit, safeDetail: "tool output exceeded the configured limit"}
	}
}

func (o *boundedOutput) wasExceeded() bool {
	select {
	case <-o.exceeded:
		return true
	default:
		return false
	}
}

func killProcessGroup(pid int) {
	// The child starts a new process group, so a negative pid reaches bunx and
	// any package-manager or ccusage descendants it spawned. Wait above always
	// reaps the direct child. ESRCH only means it exited during the race.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
