package server

import (
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/localaddr"
	"github.com/hughescr/utraque/internal/obs"
)

// listenerClassKey carries the localaddr class of the accepting listener.
type listenerClassKey struct{}

// allowedHost checks an HTTP authority, not a URL, against the loopback names:
// localhost, a 127.0.0.0/8 literal, or ::1. Bound all scans by the maximum DNS
// name plus port; never resolve a name or trust a proxy header. Bare IPv6 is
// accepted as an address, never split at its last colon.
func allowedHost(authority string) bool {
	if len(authority) == 0 || len(authority) > 261 {
		return false
	}
	host, port := authority, ""
	if authority[0] == '[' {
		var suffix string
		var ok bool
		host, suffix, ok = strings.Cut(authority[1:], "]")
		if !ok || !strings.Contains(host, ":") {
			return false
		}
		if suffix != "" {
			if suffix[0] != ':' || len(suffix) == 1 {
				return false
			}
			port = suffix[1:]
		}
	} else if h, p, ok := strings.Cut(authority, ":"); ok && !strings.Contains(p, ":") {
		if p == "" {
			return false
		}
		host, port = h, p
	}
	for i := range len(port) {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	for i := range len(host) {
		if host[i] >= 0x80 {
			return false // Case folding is ASCII-only; EqualFold also folds U+017F to "s".
		}
	}
	return localaddr.LoopbackHost(host)
}

// listenerClass is the class of the socket the request arrived on.
// HTTPServer supplies the actual listener classification. When mounted as a
// handler behind a different http.Server, use its connection-local address;
// a direct handler call carries neither, and its class is unclassifiable.
func listenerClass(r *http.Request) string {
	ctx := r.Context()
	if class, ok := ctx.Value(listenerClassKey{}).(string); ok {
		return class
	}
	addr, _ := ctx.Value(http.LocalAddrContextKey).(net.Addr)
	return localaddr.BoundClass(addr)
}

// hostCheckApplies decides whether the Host allowlist is enforced. The rule:
// only a listener positively classified as non-loopback (a wildcard or a
// routable address, where remote clients legitimately use any Host and the
// local token is mandatory) skips it. Loopback and unclassifiable listeners
// both enforce it, so an unknown listener fails closed.
func hostCheckApplies(r *http.Request) bool {
	switch listenerClass(r) {
	case localaddr.Wildcard, localaddr.NonLoopback:
		return false
	default:
		return true
	}
}

func (s *Server) withBrowserGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rule, message string
		if _, present := r.Header["Origin"]; present {
			rule, message = "origin", "requests with Origin are not accepted; use a non-browser client"
		} else if sites, present := r.Header["Sec-Fetch-Site"]; present && (len(sites) != 1 || sites[0] != "same-origin" && sites[0] != "none") {
			rule, message = "fetch-site", "cross-site or unrecognised fetch context; use a non-browser client"
		} else if hostCheckApplies(r) && !allowedHost(r.Host) {
			rule, message = "host", "unrecognised Host; use http://127.0.0.1:PORT or http://localhost:PORT, where PORT is the port utraque listens on"
		} else {
			next.ServeHTTP(w, r)
			return
		}
		s.log.LogAttrs(r.Context(), slog.LevelWarn, "request refused", slog.String("rule", rule), slog.String("path", obs.SafePath(r.URL)))
		w.Header().Set("Cache-Control", "no-store")
		_ = apierr.Write(w, apierr.New(apierr.TypePermission, "%s", message))
	})
}
