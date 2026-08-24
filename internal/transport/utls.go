package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// The uTLS transport exists for exactly one failure mode: Cloudflare deciding
// that a stock Go TLS ClientHello is a bot and answering chatgpt.com's Codex
// backend with a challenge page instead of the API. Nothing about the request
// changes — the originator stays codex_cli_rs, no browser User-Agent is
// invented, no cookie is forged. Only the shape of the TLS handshake changes.
//
// It is NOT the default. The std transport works today, it is the one whose
// semantics the rest of the proxy was built and verified against, and a
// hand-rolled TLS stack is a strictly larger attack surface. This is the
// break-glass path.
//
// Differences from NewStd that callers must know about:
//
//   - No HTTP proxy support. net/http dials cm.addr() through DialTLSContext
//     when a custom TLS dialer is set, so an https request routed via an HTTP
//     proxy would end up handshaking with the proxy rather than CONNECTing
//     through it. Proxy is therefore left nil, and Chrome-fingerprinting a
//     corporate proxy would be pointless anyway.
//   - HTTP/2 is served by x/net/http2 rather than net/http's built-in bundle,
//     because *utls.UConn is not a *tls.Conn and net/http's ALPN handoff
//     requires that concrete type. See utlsRoundTripper.

// chromeALPN is the protocol list this transport negotiates. It matches the
// Chrome preset's own ALPN extension, which is part of the fingerprint — JA4
// hashes the first offered protocol — so it is offered unconditionally, even to
// a host already known to answer only HTTP/1.1.
var chromeALPN = []string{http2.NextProtoTLS, "http/1.1"}

// errNotHTTP2 is returned by the HTTP/2 dialer when the peer declined h2 at
// ALPN. It is a routing signal inside this package, never surfaced to a caller.
var errNotHTTP2 = errors.New("utraque/transport: peer did not negotiate HTTP/2")

// utlsDialer performs the TCP dial and the Chrome-shaped TLS handshake. It
// holds no per-connection state, so one instance serves both the HTTP/1.1 and
// the HTTP/2 transport and every connection they pool.
type utlsDialer struct {
	net              *net.Dialer
	roots            *x509.CertPool
	handshakeTimeout time.Duration
}

// dial returns a handshaken uTLS connection. On any failure the underlying TCP
// connection is closed before the error is returned, so a failed handshake
// never leaks a socket.
func (d *utlsDialer) dial(ctx context.Context, network, addr string) (*utls.UConn, error) {
	raw, err := d.net.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		host = addr
	}

	cfg := &utls.Config{
		ServerName: host,
		// The Chrome preset carries its own ALPN extension; setting the same
		// list here keeps the post-handshake check of the server's selection
		// consistent with what was actually offered. Certificate verification
		// is left on: this is a fingerprint change, not a trust change.
		NextProtos: chromeALPN,
		RootCAs:    d.roots,
	}

	conn := utls.UClient(raw, cfg, utls.HelloChrome_Auto)

	hctx := ctx
	if d.handshakeTimeout > 0 {
		var cancel context.CancelFunc
		hctx, cancel = context.WithTimeout(ctx, d.handshakeTimeout)
		defer cancel()
	}
	if err := conn.HandshakeContext(hctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}

// utlsRoundTripper routes each request to the HTTP/1.1 or the HTTP/2 transport.
//
// net/http cannot do this itself here. Its ALPN handoff (transport.go:
// `pconn.conn.(*tls.Conn)`) requires the connection returned by
// DialTLSContext to be a concrete *tls.Conn; a *utls.UConn is not, so net/http
// silently speaks HTTP/1.1 over a connection the server negotiated as h2 —
// which hangs. So h2 is dialled and driven by x/net/http2 instead, whose
// DialTLSContext hook takes a plain net.Conn.
//
// Dispatch is optimistic: https starts on HTTP/2, because every host this proxy
// talks to does, and only a peer that declines h2 at ALPN demotes its authority
// to HTTP/1.1 for the rest of the process. The demotion is recorded by the h2
// dialer itself rather than inferred from the returned error, so it does not
// depend on how x/net/http2 wraps a dial failure.
type utlsRoundTripper struct {
	h1 *http.Transport
	h2 *http2.Transport

	// headerTimeout mirrors http.Transport.ResponseHeaderTimeout, which
	// x/net/http2 has no equivalent for. Zero disables it.
	headerTimeout time.Duration

	mu     sync.Mutex
	h1Only map[string]bool
}

func (rt *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Plain HTTP has no ClientHello to shape, so it is just an ordinary
	// net/http request. It is only reachable from tests and a self-hosted
	// upstream override; the real Codex backend is https.
	if req.URL == nil || req.URL.Scheme != "https" {
		return rt.h1.RoundTrip(req)
	}

	key := authorityOf(req.URL)
	if rt.isH1Only(key) {
		return rt.h1.RoundTrip(req)
	}

	resp, err := rt.roundTripH2(req)
	if err == nil {
		return resp, nil
	}
	// Retry on HTTP/1.1 only when the h2 dialer itself recorded the demotion.
	// Any other failure is a real failure and is reported as one.
	if !errors.Is(err, errNotHTTP2) && !rt.isH1Only(key) {
		return nil, err
	}
	rt.markH1Only(key)

	retry, ok := rewind(req)
	if !ok {
		return nil, err
	}
	return rt.h1.RoundTrip(retry)
}

// roundTripH2 runs the request over HTTP/2, bounding the wait for the response
// headers the way http.Transport.ResponseHeaderTimeout does for HTTP/1.1.
// Without it a silent upstream could pin a request, a connection and the idle
// timer indefinitely, since the proxy deliberately sets no overall client
// timeout (an SSE stream may legitimately run for many minutes).
func (rt *utlsRoundTripper) roundTripH2(req *http.Request) (*http.Response, error) {
	if rt.headerTimeout <= 0 {
		return rt.h2.RoundTrip(req)
	}

	ctx, cancel := context.WithCancel(req.Context())
	timer := time.AfterFunc(rt.headerTimeout, cancel)

	resp, err := rt.h2.RoundTrip(req.WithContext(ctx))
	if err != nil {
		timer.Stop()
		cancel()
		return nil, err
	}
	timer.Stop()
	// The headers arrived, so the deadline has done its job; from here the
	// stream runs until the body is closed or the caller's own context ends.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

func (rt *utlsRoundTripper) isH1Only(key string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.h1Only[key]
}

func (rt *utlsRoundTripper) markH1Only(key string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.h1Only[key] = true
}

// CloseIdleConnections drains both pools, so http.Client.CloseIdleConnections
// keeps working through this composite RoundTripper.
func (rt *utlsRoundTripper) CloseIdleConnections() {
	rt.h1.CloseIdleConnections()
	rt.h2.CloseIdleConnections()
}

// cancelOnClose releases a per-request context once the body is closed, so the
// header deadline's cancel func cannot outlive the response it guarded.
type cancelOnClose struct {
	io.ReadCloser
	once   sync.Once
	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cancel)
	return err
}

// rewind prepares req for a second attempt on the other protocol. A bodyless
// request can always be replayed; a request with a body can only be replayed
// when net/http gave it a GetBody (it does for the in-memory bodies this proxy
// sends). Anything else is reported as unreplayable rather than silently sent
// with a drained body.
func rewind(req *http.Request) (*http.Request, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	clone := req.Clone(req.Context())
	clone.Body = body
	return clone, true
}

// authorityOf is the host:port key both dispatch halves agree on. req.URL.Host
// omits the default port while the dialer's addr always carries one, so they
// must be normalised to the same shape or a demotion recorded by the dialer
// would never be seen by RoundTrip.
func authorityOf(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "http" {
		return net.JoinHostPort(u.Hostname(), "80")
	}
	return net.JoinHostPort(u.Hostname(), "443")
}

type utlsTransport struct {
	client *http.Client
}

func (t *utlsTransport) Client() *http.Client { return t.client }

func (t *utlsTransport) Kind() string { return KindUTLS }

// NewUTLS builds the Chrome-fingerprint transport.
//
// It holds the two properties NewStd documents as load-bearing — no redirect is
// ever followed, and there is no overall client timeout — and honours the same
// Options, including DisableCompression on both protocol paths.
//
// No network access happens here: the fingerprint is chosen per connection, so
// construction is cheap enough for auto mode to build one up front and keep it
// unused unless a gate is ever reported.
func NewUTLS(opts Options) Transport {
	o := opts.normalize()

	dialer := &utlsDialer{
		net: &net.Dialer{
			Timeout:   o.DialTimeout,
			KeepAlive: 30 * time.Second,
		},
		roots:            o.RootCAs,
		handshakeTimeout: o.TLSHandshakeTimeout,
	}

	rt := &utlsRoundTripper{
		headerTimeout: o.ResponseHeaderTimeout,
		h1Only:        make(map[string]bool),
	}

	rt.h1 = &http.Transport{
		// Proxy is deliberately unset — see the package note above.
		DialContext: dialer.net.DialContext,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.dial(ctx, network, addr)
		},
		// The h2 half is x/net/http2's, reached through utlsRoundTripper, so
		// net/http must not try its own ALPN upgrade on a connection it cannot
		// recognise as TLS.
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   o.TLSHandshakeTimeout,
		ResponseHeaderTimeout: o.ResponseHeaderTimeout,
		IdleConnTimeout:       o.IdleConnTimeout,
		MaxIdleConns:          o.MaxIdleConns,
		MaxIdleConnsPerHost:   o.MaxIdleConnsPerHost,
		DisableCompression:    o.DisableCompression,
		ExpectContinueTimeout: 1 * time.Second,
	}

	rt.h2 = &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			conn, err := dialer.dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if proto := conn.ConnectionState().NegotiatedProtocol; proto != http2.NextProtoTLS {
				// Record the demotion here rather than leaving RoundTrip to
				// decode it out of whatever error x/net/http2 returns.
				rt.markH1Only(addr)
				_ = conn.Close()
				return nil, errNotHTTP2
			}
			return conn, nil
		},
		DisableCompression: o.DisableCompression,
		IdleConnTimeout:    o.IdleConnTimeout,
		// Keep a long-lived stream honest: ping a silent connection and fail it
		// rather than holding a stream open on a peer that has gone away.
		ReadIdleTimeout: o.IdleConnTimeout,
		PingTimeout:     o.TLSHandshakeTimeout,
	}

	return &utlsTransport{
		client: &http.Client{
			Transport:     rt,
			CheckRedirect: noRedirect,
		},
	}
}
