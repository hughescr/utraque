//go:build unix

package auth

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// errLockTimeout is returned when the advisory lock cannot be taken in time.
var errLockTimeout = errors.New("auth: timed out waiting for auth.json.lock")

// acquireFileLock takes an exclusive advisory (flock) lock on path, creating it
// if absent, and returns a release function. It polls with LOCK_NB so it can
// honour a deadline rather than blocking forever, because the Codex CLI may
// hold the lock while it writes the same auth.json. The lock is advisory: it
// coordinates only with other processes that also flock this sibling file.
//
// It also honours ctx: if the caller's request is cancelled while we are still
// waiting for the lock, we stop waiting and return ctx.Err() rather than pinning
// the goroutine for the whole timeout.
func acquireFileLock(ctx context.Context, path string, timeout time.Duration) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			_ = f.Close()
			return nil, ctxErr
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, errLockTimeout
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
