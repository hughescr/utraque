package localaddr

import (
	"net"
	"net/netip"
	"testing"
)

func TestBoundClass(t *testing.T) {
	for _, tc := range []struct{ ip, want string }{
		{"127.0.0.2", Loopback}, {"::1", Loopback}, {"::ffff:127.0.0.1", "non-loopback"}, {"::", "wildcard"}, {"0.0.0.0", "wildcard"}, {"192.0.2.1", "non-loopback"},
	} {
		if got := BoundClass(&net.TCPAddr{IP: netip.MustParseAddr(tc.ip).AsSlice()}); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.ip, got, tc.want)
		}
	}
	for _, a := range []net.Addr{nil, (*net.TCPAddr)(nil), &net.TCPAddr{IP: net.IP{127}}, &net.UnixAddr{}} {
		if got := BoundClass(a); got != "unclassifiable" {
			t.Errorf("%T: got %s", a, got)
		}
	}
}

func TestInheritedLoopbackFamilies(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
			if err != nil {
				t.Skipf("family unavailable: %v", err)
			}
			defer ln.Close()
			f, err := ln.(*net.TCPListener).File()
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			inherited, err := net.FileListener(f)
			if err != nil {
				t.Fatal(err)
			}
			defer inherited.Close()
			t.Logf("inherited IP bytes: %x", []byte(inherited.Addr().(*net.TCPAddr).IP))
			if BoundClass(inherited.Addr()) != Loopback {
				t.Errorf("refused inherited %s", host)
			}
		})
	}
}
