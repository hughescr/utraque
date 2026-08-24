//go:build !darwin || !cgo

package launchd

import "net"

// activateSocket is the stub for every build that cannot call
// launch_activate_socket: any non-darwin platform, and darwin built with
// CGO_ENABLED=0. It always reports ErrNotSupported, which Listen treats exactly
// like "no socket here" and answers with the plain-listen fallback — so a
// cross-compiled or cgo-less binary still runs, it just cannot be socket
// activated.
func activateSocket(string) ([]net.Listener, error) { return nil, ErrNotSupported }
