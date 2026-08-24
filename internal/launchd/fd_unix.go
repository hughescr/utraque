//go:build unix

package launchd

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// listenersFromFDs adopts raw inherited descriptors as listeners.
//
// net.FileListener duplicates the descriptor it is given and registers the
// duplicate with the runtime poller, so the original is surplus the moment the
// call succeeds and is closed here. Failing to close it would leak one
// descriptor per socket for the life of the process; failing to close the
// descriptors we never reached would leak the rest, so both error paths sweep
// up after themselves.
//
// A descriptor that is not a listening socket — a plist Sockets entry naming a
// path, say — makes net.FileListener fail, and that failure is returned rather
// than skipped: silently dropping one of the addresses launchd bound would mean
// requests to it hang forever with nothing accepting.
func listenersFromFDs(name string, fds []int) ([]net.Listener, error) {
	lns := make([]net.Listener, 0, len(fds))
	for i, fd := range fds {
		// launchd sets close-on-exec on what it hands over, but say so anyway:
		// utraque must never leak its listening socket into a child process.
		syscall.CloseOnExec(fd)

		f := os.NewFile(uintptr(fd), fmt.Sprintf("launchd-socket:%s#%d", name, i))
		if f == nil {
			closeFDs(fds[i:])
			_ = CloseAll(lns)
			return nil, fmt.Errorf("launchd: socket %q descriptor %d is not a valid file descriptor", name, fd)
		}
		ln, err := net.FileListener(f)
		_ = f.Close() // FileListener dup'd it; on failure this is the only close.
		if err != nil {
			closeFDs(fds[i+1:])
			_ = CloseAll(lns)
			return nil, fmt.Errorf("launchd: socket %q descriptor %d is not a listening socket: %w", name, fd, err)
		}
		lns = append(lns, ln)
	}
	if len(lns) == 0 {
		return nil, fmt.Errorf("%w: launchd returned no usable descriptors for socket %q", ErrNotManaged, name)
	}
	return lns, nil
}

// closeFDs releases descriptors we took ownership of but never wrapped.
func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
}
