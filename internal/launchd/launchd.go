// Package launchd turns a macOS launchd-held listening socket into a
// net.Listener, and falls back to binding the configured address when there is
// no launchd socket to inherit.
//
// The point of the exercise is on-demand service: the plist declares a Sockets
// entry, launchd owns the listening socket and holds it open forever, and
// utraque is only started when a connection actually arrives. Combined with the
// idle timer in internal/idle, the daemon costs nothing between sessions —
// launchd re-activates it on the next request, so it stays seamless.
//
// Pure Go cannot ask launchd for the descriptors: launch_activate_socket is a
// C function with no syscall equivalent, so the darwin build carries a small
// cgo shim (activate_darwin.go). Every other platform, and darwin with cgo
// disabled, gets the stub in activate_unsupported.go, which reports
// ErrNotSupported and lands on the plain-listen fallback. "go run" and a manual
// start therefore behave exactly as they did before socket activation existed.
package launchd

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
)

// DefaultSocketName is the key utraque's plist uses in its Sockets dictionary.
// launchd addresses inherited sockets by that key, not by port.
const DefaultSocketName = "Listener"

// Source records where a set of listeners came from, so the caller can log it
// and decide policy — notably whether idle self-exit is safe to enable.
type Source string

const (
	// SourceLaunchd means launchd owns the socket and will re-activate us.
	SourceLaunchd Source = "launchd"
	// SourceListen means we bound the address ourselves. Exiting when idle
	// would leave nothing listening, so the caller must not enable self-exit
	// on its own account.
	SourceListen Source = "listen"
)

// String renders the source for logs.
func (s Source) String() string { return string(s) }

var (
	// ErrNotSupported means this build cannot perform socket activation at
	// all: a non-darwin platform, or darwin built with CGO_ENABLED=0.
	ErrNotSupported = errors.New("launchd: socket activation is not supported by this build")

	// ErrNotManaged means the call worked but this process holds no launchd
	// socket under that name — the normal case for a manual start, and also
	// what a plist missing the Sockets key produces.
	ErrNotManaged = errors.New("launchd: this process holds no socket by that name")
)

// Options configures Listen.
type Options struct {
	// SocketName is the plist Sockets key to activate. Empty means
	// DefaultSocketName.
	SocketName string

	// Addr is the address to bind when launchd holds no socket. Empty makes a
	// failed activation fatal instead of falling back, which is what a caller
	// that only ever wants socket activation should ask for.
	Addr string

	// Logger receives one line explaining which path was taken. Optional.
	Logger *slog.Logger

	// activate is the platform hook, replaced in tests. Nil means the real
	// one for this build.
	activate func(name string) ([]net.Listener, error)
}

// Listen returns the listeners to serve on, preferring the sockets launchd
// already holds and falling back to binding opts.Addr.
//
// launchd hands back one descriptor per address family it bound for the entry,
// so the result is a slice: a Sockets entry with no SockNodeName yields both an
// IPv4 and an IPv6 listener, and serving only the first would silently ignore
// half the traffic. The caller owns every returned listener.
func Listen(opts Options) ([]net.Listener, Source, error) {
	name := opts.SocketName
	if name == "" {
		name = DefaultSocketName
	}
	act := opts.activate
	if act == nil {
		act = activateSocket
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	lns, err := act(name)
	switch {
	case err == nil:
		addrs := make([]string, len(lns))
		for i, ln := range lns {
			addrs[i] = ln.Addr().String()
		}
		log.Info("inherited the listening socket from launchd",
			slog.String("socket", name), slog.Any("addrs", addrs))
		return lns, SourceLaunchd, nil

	case errors.Is(err, ErrNotManaged), errors.Is(err, ErrNotSupported):
		// Not an error: this is the manual-start path. Only a missing fallback
		// address turns it into one.
		if opts.Addr == "" {
			return nil, SourceListen, fmt.Errorf("launchd: no socket %q and no fallback address configured: %w", name, err)
		}
		ln, lerr := net.Listen("tcp", opts.Addr)
		if lerr != nil {
			return nil, SourceListen, lerr
		}
		log.Info("no launchd socket; listening on the configured address",
			slog.String("socket", name),
			slog.String("addr", ln.Addr().String()),
			slog.String("reason", err.Error()))
		return []net.Listener{ln}, SourceListen, nil

	default:
		// launchd answered with something else — the socket exists but we could
		// not take it (already activated, a descriptor that is not a socket).
		// Binding the address ourselves would collide with the socket launchd
		// still holds, so fail loudly rather than half-work.
		return nil, SourceLaunchd, err
	}
}

// CloseAll closes every listener, reporting the first failure. It is the
// cleanup path for a caller that acquired listeners and then failed to build
// the server that was going to serve them.
func CloseAll(lns []net.Listener) error {
	var first error
	for _, ln := range lns {
		if ln == nil {
			continue
		}
		if err := ln.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
