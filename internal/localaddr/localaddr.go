// Package localaddr classifies listener addresses and literal loopback hosts.
package localaddr

import (
	"net"
	"net/netip"
	"strings"
)

// The classes BoundClass returns. Loopback is the only one safe to serve
// without a local token. Unclassifiable means the address is not a usable TCP
// address, so nothing is known about where the socket listens.
const (
	Loopback       = "loopback"
	Wildcard       = "wildcard"
	NonLoopback    = "non-loopback"
	Unclassifiable = "unclassifiable"
)

// BoundClass describes the actual listening socket, never the configured bind
// spelling or the endpoint of an accepted connection.
func BoundClass(addr net.Addr) string {
	a, ok := addr.(*net.TCPAddr)
	if !ok || a == nil {
		return Unclassifiable
	}
	ip, ok := netip.AddrFromSlice(a.IP)
	if !ok {
		return Unclassifiable
	}
	// A real IPv4 bind (also via [::ffff:127.0.0.1] on Darwin) reports
	// four bytes. Do not reinterpret a 16-byte IPv6 listener as IPv4.
	switch {
	case literalLoopback(ip):
		return Loopback
	case ip.IsUnspecified():
		return Wildcard
	default:
		return NonLoopback
	}
}

// LoopbackHost accepts localhost, literal 127/8, or ::1, without a lookup.
// Unlike a kernel socket address, user-supplied text must not be unmapped:
// shorthand, IPv4-mapped IPv6 and scoped addresses are not accepted.
func LoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && literalLoopback(ip)
}

func literalLoopback(ip netip.Addr) bool {
	return (ip.Is4() && ip.As4()[0] == 127) || ip == netip.IPv6Loopback()
}
