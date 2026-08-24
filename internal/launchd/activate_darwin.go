//go:build darwin && cgo

package launchd

/*
#include <launch.h>
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// activateSocket asks launchd for the descriptors it holds under name.
//
// launch_activate_socket is the only supported way to reach them: the
// descriptors are not at fixed numbers, and the older launch_data API has been
// removed. It allocates the descriptor array with malloc, so the caller frees
// it; the descriptors themselves become ours.
//
// Its return value is an errno, not -1/errno:
//
//	ESRCH   — this process is not managed by launchd (a manual start)
//	ENOENT  — launchd manages us, but the job declares no socket by that name
//	EALREADY — the socket was already activated in this process
//
// The first two are the ordinary "no socket here" answers and map to
// ErrNotManaged so the caller falls back to a plain listen. Anything else is a
// real failure and is reported as one.
func activateSocket(name string) ([]net.Listener, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	var (
		cfds  *C.int
		count C.size_t
	)
	if rc := C.launch_activate_socket(cname, &cfds, &count); rc != 0 {
		errno := syscall.Errno(rc)
		switch errno {
		case syscall.ESRCH, syscall.ENOENT:
			return nil, fmt.Errorf("%w: launch_activate_socket(%q): %v", ErrNotManaged, name, errno)
		default:
			return nil, fmt.Errorf("launchd: launch_activate_socket(%q): %w", name, errno)
		}
	}
	// launch_activate_socket allocated the array even when it reports success
	// with zero descriptors; free it either way. free(NULL) is defined.
	defer C.free(unsafe.Pointer(cfds))

	n := int(count)
	if n == 0 || cfds == nil {
		return nil, fmt.Errorf("%w: launchd returned no descriptors for socket %q", ErrNotManaged, name)
	}

	fds := make([]int, 0, n)
	for _, fd := range unsafe.Slice(cfds, n) {
		fds = append(fds, int(fd))
	}
	return listenersFromFDs(name, fds)
}
