//go:build unix

package launchd

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// inheritedFD forges what launchd hands over: a bare descriptor number for an
// already-bound, already-listening socket, owned by the caller. It is the
// closest a test can get to socket activation without launchd.
func inheritedFD(t *testing.T) (fd int, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr = ln.Addr().String()

	tcp, ok := ln.(*net.TCPListener)
	if !ok {
		t.Fatalf("listener is %T, want *net.TCPListener", ln)
	}
	f, err := tcp.File() // a duplicate of the listening socket
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	defer func() { _ = f.Close() }()
	// Closing the runtime's copy leaves the socket alive through f: descriptors
	// share one open file description, and only the last close destroys it.
	_ = ln.Close()

	// Hand out a descriptor of our own so ownership is unambiguous: f stays
	// ours to close, the dup passes to the code under test.
	fd, err = syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	return fd, addr
}

// closed reports whether a descriptor number is no longer open.
func closed(fd int) bool {
	var st syscall.Stat_t
	return syscall.Fstat(fd, &st) == syscall.EBADF
}

func TestListenersFromFDsAdoptsAnInheritedSocket(t *testing.T) {
	fd, addr := inheritedFD(t)

	lns, err := listenersFromFDs("Listener", []int{fd})
	if err != nil {
		t.Fatalf("listenersFromFDs: %v", err)
	}
	defer func() { _ = CloseAll(lns) }()

	if len(lns) != 1 {
		t.Fatalf("got %d listeners, want 1", len(lns))
	}
	if got := lns[0].Addr().String(); got != addr {
		t.Errorf("addr = %q, want the inherited socket's %q", got, addr)
	}
	// net.FileListener duplicates what it is given, so the descriptor we passed
	// in must have been closed rather than left to leak for the process's life.
	if !closed(fd) {
		t.Error("the inherited descriptor leaked: it is still open after FileListener dup'd it")
	}
	if err := echoOnce(t, lns[0]); err != nil {
		t.Errorf("the adopted listener did not accept: %v", err)
	}
}

func TestListenersFromFDsAdoptsSeveralSockets(t *testing.T) {
	// launchd binds one socket per address family for a Sockets entry with no
	// SockNodeName, so adopting a set is the normal case, not an edge one.
	fd1, addr1 := inheritedFD(t)
	fd2, addr2 := inheritedFD(t)

	lns, err := listenersFromFDs("Listener", []int{fd1, fd2})
	if err != nil {
		t.Fatalf("listenersFromFDs: %v", err)
	}
	defer func() { _ = CloseAll(lns) }()

	if len(lns) != 2 {
		t.Fatalf("got %d listeners, want 2", len(lns))
	}
	for i, want := range []string{addr1, addr2} {
		if got := lns[i].Addr().String(); got != want {
			t.Errorf("listener %d addr = %q, want %q", i, got, want)
		}
		if err := echoOnce(t, lns[i]); err != nil {
			t.Errorf("listener %d did not accept: %v", i, err)
		}
	}
}

func TestListenersFromFDsRejectsANonSocket(t *testing.T) {
	// A plist Sockets entry that names something other than a listening socket
	// must fail loudly: quietly dropping an address launchd bound would leave
	// connections to it hanging with nothing to accept them.
	path := filepath.Join(t.TempDir(), "not-a-socket")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	lns, err := listenersFromFDs("Listener", []int{fd})
	if err == nil {
		_ = CloseAll(lns)
		t.Fatal("a non-socket descriptor must be an error")
	}
	if !strings.Contains(err.Error(), "Listener") {
		t.Errorf("err = %v, want it to name the socket", err)
	}
	if !closed(fd) {
		t.Error("the rejected descriptor leaked")
	}
}

func TestListenersFromFDsSweepsUpAfterAFailure(t *testing.T) {
	good, _ := inheritedFD(t)
	path := filepath.Join(t.TempDir(), "not-a-socket")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	bad, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	trailing, _ := inheritedFD(t)

	if _, err := listenersFromFDs("Listener", []int{good, bad, trailing}); err == nil {
		t.Fatal("want an error from the middle descriptor")
	}
	// Every descriptor we took ownership of is released: the one already
	// wrapped, the one that failed, and the one never reached.
	for name, fd := range map[string]int{"good": good, "bad": bad, "trailing": trailing} {
		if !closed(fd) {
			t.Errorf("the %s descriptor leaked after the failure", name)
		}
	}
}

func TestListenersFromFDsRejectsAnEmptySet(t *testing.T) {
	_, err := listenersFromFDs("Listener", nil)
	if err == nil {
		t.Fatal("no descriptors must be an error")
	}
	// It reports "not managed" so Listen falls back to a plain bind rather than
	// treating an empty answer as fatal.
	if !errors.Is(err, ErrNotManaged) {
		t.Errorf("err = %v, want it to wrap ErrNotManaged", err)
	}
}
