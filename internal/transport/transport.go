// Package transport builds the HTTP clients the upstream legs use. It exists
// as an interface so a uTLS Chrome-impersonation dialer can replace the
// standard one if Cloudflare ever fingerprint-gates an upstream.
package transport

import (
	"net"
	"net/http"
	"time"
)

// Kinds of HTTP transport. KindUTLS is reserved for the phase-8 Cloudflare
// fallback; only KindStd is implemented today.
const (
	KindStd  = "std"
	KindUTLS = "utls"
)

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
	return &stdTransport{
		client: &http.Client{
			Transport: ht,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}
