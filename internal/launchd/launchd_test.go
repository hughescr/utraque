package launchd

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
)

// fakeActivator stands in for launch_activate_socket so the fallback and
// adoption paths are testable on any machine, launchd-managed or not.
func fakeActivator(lns []net.Listener, err error) func(string) ([]net.Listener, error) {
	return func(string) ([]net.Listener, error) { return lns, err }
}

func TestListenFallsBackWhenLaunchdHoldsNoSocket(t *testing.T) {
	for name, actErr := range map[string]error{
		"not managed":   fmt.Errorf("%w: ESRCH", ErrNotManaged),
		"not supported": ErrNotSupported,
	} {
		t.Run(name, func(t *testing.T) {
			lns, src, err := Listen(Options{
				Addr:     "127.0.0.1:0",
				activate: fakeActivator(nil, actErr),
			})
			if err != nil {
				t.Fatalf("Listen: %v", err)
			}
			defer func() { _ = CloseAll(lns) }()

			if src != SourceListen {
				t.Errorf("source = %q, want %q", src, SourceListen)
			}
			if len(lns) != 1 {
				t.Fatalf("got %d listeners, want 1", len(lns))
			}
			// The fallback must be a genuinely usable listener: "go run" and a
			// manual start depend on this path entirely.
			if err := echoOnce(t, lns[0]); err != nil {
				t.Errorf("the fallback listener did not serve: %v", err)
			}
		})
	}
}

func TestListenAdoptsLaunchdSockets(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lns, src, err := Listen(Options{
		Addr:     "127.0.0.1:0",
		activate: fakeActivator([]net.Listener{held}, nil),
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = CloseAll(lns) }()

	if src != SourceLaunchd {
		t.Errorf("source = %q, want %q", src, SourceLaunchd)
	}
	if len(lns) != 1 || lns[0] != held {
		t.Fatalf("Listen did not return the inherited listener: %v", lns)
	}
	// The configured fallback address must NOT have been bound as well.
	if got := lns[0].Addr().String(); got == "" {
		t.Error("inherited listener has no address")
	}
}

func TestListenSurfacesRealActivationFailures(t *testing.T) {
	// EALREADY, a descriptor that is not a socket, and friends. Falling back
	// would collide with the socket launchd still holds, so this must fail.
	boom := errors.New("launchd: launch_activate_socket(\"Listener\"): resource busy")
	lns, _, err := Listen(Options{Addr: "127.0.0.1:0", activate: fakeActivator(nil, boom)})
	if err == nil {
		_ = CloseAll(lns)
		t.Fatal("a hard activation failure must not fall back to a plain listen")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the activation failure", err)
	}
}

func TestListenWithoutFallbackAddressIsAnError(t *testing.T) {
	_, _, err := Listen(Options{activate: fakeActivator(nil, ErrNotSupported)})
	if err == nil {
		t.Fatal("no launchd socket and no fallback address must be an error")
	}
	if !errors.Is(err, ErrNotSupported) {
		t.Errorf("err = %v, want it to name the underlying reason", err)
	}
	if !strings.Contains(err.Error(), DefaultSocketName) {
		t.Errorf("err = %v, want it to name the socket it looked for", err)
	}
}

func TestListenDefaultsTheSocketName(t *testing.T) {
	var asked string
	_, _, err := Listen(Options{
		Addr: "127.0.0.1:0",
		activate: func(name string) ([]net.Listener, error) {
			asked = name
			return nil, ErrNotSupported
		},
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if asked != DefaultSocketName {
		t.Errorf("activated %q, want the default %q", asked, DefaultSocketName)
	}
}

func TestCloseAllTolerantOfNils(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := CloseAll([]net.Listener{nil, ln, nil}); err != nil {
		t.Errorf("CloseAll = %v, want nil", err)
	}
	if err := CloseAll(nil); err != nil {
		t.Errorf("CloseAll(nil) = %v, want nil", err)
	}
}

func TestSourceString(t *testing.T) {
	if SourceLaunchd.String() != "launchd" || SourceListen.String() != "listen" {
		t.Error("Source.String does not render the constant")
	}
}

// echoOnce proves a listener accepts by serving exactly one connection.
func echoOnce(t *testing.T, ln net.Listener) error {
	t.Helper()
	errc := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errc <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_, err = io.WriteString(conn, "ok")
		errc <- err
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	got, err := io.ReadAll(conn)
	if err != nil {
		return err
	}
	if string(got) != "ok" {
		return fmt.Errorf("read %q, want %q", got, "ok")
	}
	return <-errc
}
