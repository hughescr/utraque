// Package transport builds the HTTP clients the upstream legs use. It exists
// as an interface so a uTLS Chrome-fingerprint dialer can replace the standard
// one if Cloudflare ever fingerprint-gates an upstream.
//
// Three implementations exist:
//
//   - NewStd  — the standard library. The default, and the one everything else
//     was built and live-verified against.
//   - NewUTLS — a Chrome-shaped TLS ClientHello, for a fingerprint gate.
//   - NewAuto — std until a gate is reported, uTLS from then on, once.
//
// Only the TLS handshake ever differs. No implementation invents a browser
// User-Agent, a cookie, or any other header: the request identity stays the
// honest codex_cli_rs originator the Codex CLI itself sends.
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Kinds of HTTP transport. A Kind names the TLS stack that actually carried a
// request, which is what the request log records; it is not the configured
// mode (auto reports whichever of the two is live).
const (
	KindStd  = "std"
	KindUTLS = "utls"
)

// Modes are the configured transport selections, i.e. the accepted values of
// codex.transport. They are plain strings because config validates them
// without importing this package; the two lists must stay in step, which
// TestModeValuesAreStable pins on this side.
const (
	// ModeStd always uses the standard library transport.
	ModeStd = "std"
	// ModeUTLS always uses the Chrome-fingerprint transport. Diagnostic and
	// break-glass; it is never the default.
	ModeUTLS = "utls"
	// ModeAuto starts on std and switches to uTLS, once, if the upstream ever
	// answers with a bot/TLS gate. This is the default.
	ModeAuto = "auto"
)

// honestOriginator is the client identity the Codex leg sends. It is named in
// the auto-switch warning to make the scope of the switch explicit: uTLS
// changes the TLS handshake and nothing else.
const honestOriginator = "codex_cli_rs"

// New builds the transport named by mode. An empty mode is ModeAuto.
//
// log receives the auto transport's switch warning; nil means slog.Default.
func New(mode string, opts Options, log *slog.Logger) (Transport, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ModeAuto:
		return NewAuto(opts, log), nil
	case ModeStd:
		return NewStd(opts), nil
	case ModeUTLS:
		return NewUTLS(opts), nil
	default:
		return nil, fmt.Errorf("utraque/transport: unknown mode %q: want %s|%s|%s", mode, ModeAuto, ModeStd, ModeUTLS)
	}
}

// noRedirect is the redirect policy every implementation shares: report the 3xx
// to the caller rather than following it. Following one would silently re-send
// the caller's Authorization bearer token to whatever host upstream named.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Transport hands out a configured *http.Client. Implementations build the
// client once and return the same instance from every Client() call so
// connection pooling is shared process-wide.
type Transport interface {
	Client() *http.Client
	Kind() string
}

// Options tunes the standard transport. A zero field takes the DefaultOptions
// value; a negative duration or count is treated as zero (i.e. defaulted).
type Options struct {
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
	MaxIdleConns          int
	MaxIdleConnsPerHost   int

	// DisableCompression stops the transport adding its own Accept-Encoding
	// and transparently gunzipping. Passthrough legs need this: the client's
	// own Accept-Encoding is forwarded and the upstream body must reach the
	// client byte-for-byte, still encoded as upstream sent it.
	DisableCompression bool

	// RootCAs replaces the system trust store. Nil — always, in production —
	// means the system store. It exists so a test can point a transport at an
	// httptest TLS server, which is the only way to exercise the uTLS handshake
	// without contacting a real host. It can only ADD trust for a named CA; no
	// option here can switch certificate verification off.
	RootCAs *x509.CertPool
}

// DefaultOptions returns the tuned defaults used by the proxy.
func DefaultOptions() Options {
	return Options{
		DialTimeout:           10 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		DisableCompression:    true,
	}
}

// normalize fills zero/negative fields from DefaultOptions. DisableCompression
// is a bool and cannot be distinguished from "unset", so it is taken as given;
// callers should start from DefaultOptions().
func (o Options) normalize() Options {
	d := DefaultOptions()
	if o.DialTimeout <= 0 {
		o.DialTimeout = d.DialTimeout
	}
	if o.TLSHandshakeTimeout <= 0 {
		o.TLSHandshakeTimeout = d.TLSHandshakeTimeout
	}
	if o.ResponseHeaderTimeout <= 0 {
		o.ResponseHeaderTimeout = d.ResponseHeaderTimeout
	}
	if o.IdleConnTimeout <= 0 {
		o.IdleConnTimeout = d.IdleConnTimeout
	}
	if o.MaxIdleConns <= 0 {
		o.MaxIdleConns = d.MaxIdleConns
	}
	if o.MaxIdleConnsPerHost <= 0 {
		o.MaxIdleConnsPerHost = d.MaxIdleConnsPerHost
	}
	return o
}

type stdTransport struct {
	client *http.Client
}

func (t *stdTransport) Client() *http.Client { return t.client }

func (t *stdTransport) Kind() string { return KindStd }

// NewStd builds the standard library transport.
//
// Two properties are load-bearing for the proxy and are not configurable:
//
//   - CheckRedirect always returns http.ErrUseLastResponse, so a redirect is
//     never followed. The 3xx and its Location reach the client unchanged;
//     following one would silently re-send the caller's Authorization bearer
//     token to whatever host upstream named.
//   - Client.Timeout is zero. An overall deadline would cut long SSE streams
//     mid-flight; per-request deadlines come from the request context, and
//     ResponseHeaderTimeout bounds the pre-first-byte wait.
func NewStd(opts Options) Transport {
	o := opts.normalize()
	ht := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   o.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   o.TLSHandshakeTimeout,
		ResponseHeaderTimeout: o.ResponseHeaderTimeout,
		IdleConnTimeout:       o.IdleConnTimeout,
		MaxIdleConns:          o.MaxIdleConns,
		MaxIdleConnsPerHost:   o.MaxIdleConnsPerHost,
		DisableCompression:    o.DisableCompression,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if o.RootCAs != nil {
		ht.TLSClientConfig = &tls.Config{RootCAs: o.RootCAs, MinVersion: tls.VersionTLS12}
	}
	return &stdTransport{
		client: &http.Client{
			Transport:     ht,
			CheckRedirect: noRedirect,
		},
	}
}
