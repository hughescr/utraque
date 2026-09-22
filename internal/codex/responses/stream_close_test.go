package responses

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// roundTripFunc lets a test answer a request without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type funcTransport struct{ c *http.Client }

func (f funcTransport) Client() *http.Client { return f.c }
func (funcTransport) Kind() string           { return "std" }

// orderGuard stands in for a transport's response body. Its Read blocks until
// the request's own context ends, the way net/http's does, and it records any
// Close that lands while a Read is still inside it. It never passes an overlap
// on to a real body, so a regression fails the test instead of hanging it.
type orderGuard struct {
	ctx     context.Context
	entered chan struct{}
	free    chan struct{} // closed on an overlapping Close so the Read can return
	freeOne sync.Once

	mu          sync.Mutex
	active      bool
	overlap     bool
	closes      int
	readsClosed int
}

func (g *orderGuard) Read(p []byte) (int, error) {
	g.mu.Lock()
	if g.closes > 0 {
		g.readsClosed++
	}
	g.active = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.active = false
		g.mu.Unlock()
	}()
	select {
	case g.entered <- struct{}{}:
	default:
	}
	select {
	case <-g.ctx.Done():
		return 0, g.ctx.Err()
	case <-g.free:
		return 0, io.ErrUnexpectedEOF
	}
}

func (g *orderGuard) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closes++
	if g.active {
		g.overlap = true
		g.freeOne.Do(func() { close(g.free) })
	}
	return nil
}

// TestStreamCloseJoinsPendingReadBeforeClosingBody pins the ordering that keeps
// Go 1.27.0/1.27.1's HTTP/1 transport from stranding a reader: the underlying
// body is never closed while a Read on it is in flight, the pending Read is
// ended by cancelling this request's own context, and the caller's context is
// left alone.
func TestStreamCloseJoinsPendingReadBeforeClosingBody(t *testing.T) {
	var g *orderGuard
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		g = &orderGuard{ctx: r.Context(), entered: make(chan struct{}, 1), free: make(chan struct{})}
		return &http.Response{
			StatusCode: http.StatusOK,
			ProtoMajor: 1,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       g,
			Request:    r,
		}, nil
	})
	c := newClient(t, "http://codex.invalid", func(o *Options) {
		o.Transport = funcTransport{c: &http.Client{Transport: rt}}
	})

	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	resp, err := c.StreamResponse(parent, testCred(), testRequest())
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 64))
		readDone <- err
	}()
	select {
	case <-g.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the body Read never started")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- resp.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return: it must end the pending Read by cancelling the request")
	}
	select {
	case err := <-readDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("pending Read ended with %v, want one wrapping context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the pending Read never returned")
	}

	if err := resp.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := resp.Body.Read(make([]byte, 1)); !errors.Is(err, http.ErrBodyReadAfterClose) {
		t.Errorf("Read after Close = %v, want http.ErrBodyReadAfterClose", err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.overlap {
		t.Error("the transport body was closed while a Read on it was still in flight")
	}
	if g.closes != 1 {
		t.Errorf("transport body closed %d times, want exactly 1", g.closes)
	}
	if g.readsClosed != 0 {
		t.Errorf("%d Read(s) reached the transport body after Close", g.readsClosed)
	}
	if parent.Err() != nil {
		t.Error("closing the stream cancelled the caller's context")
	}
}

// TestStreamResponseReleasesRequestContextOnFailure checks that an attempt that
// hands no body to the caller does not leave its request context running, and
// that the refresh retry runs on a fresh one.
func TestStreamResponseReleasesRequestContextOnFailure(t *testing.T) {
	var ctxs []context.Context
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		ctxs = append(ctxs, r.Context())
		if len(ctxs) == 1 {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"expired"}}`)),
				Request:    r,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event: response.created\ndata: {}\n\n")),
			Request:    r,
		}, nil
	})
	c := newClient(t, "http://codex.invalid", func(o *Options) {
		o.Transport = funcTransport{c: &http.Client{Transport: rt}}
	})

	src := &fakeSource{tokens: []string{"tok-stale", "tok-fresh"}}
	resp, err := c.StreamResponseWithRefresh(context.Background(), src, testRequest())
	if err != nil {
		t.Fatalf("StreamResponseWithRefresh: %v", err)
	}
	if len(ctxs) != 2 {
		t.Fatalf("attempts = %d, want 2", len(ctxs))
	}
	if ctxs[0].Err() == nil {
		t.Error("the rejected attempt's request context was left running")
	}
	if ctxs[1].Err() != nil {
		t.Error("the retry's request context ended before its body was closed")
	}
	_ = resp.Close()
	if ctxs[1].Err() == nil {
		t.Error("closing the stream did not release its request context")
	}
}

// gatedConn holds every socket Read issued after arm until open is closed or the
// connection itself is closed, and reports the first such Read on entered. It
// lets a test park a body Read inside net/http at a known point.
type gatedConn struct {
	net.Conn
	mu       sync.Mutex
	armed    bool
	entered  chan struct{}
	open     chan struct{}
	closed   chan struct{}
	closeOne sync.Once
}

func (c *gatedConn) arm() {
	c.mu.Lock()
	c.armed = true
	c.mu.Unlock()
}

func (c *gatedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	armed := c.armed
	c.mu.Unlock()
	if armed {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		select {
		case <-c.open:
		case <-c.closed:
		}
	}
	return c.Conn.Read(p)
}

func (c *gatedConn) Close() error {
	c.closeOne.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// TestStreamCloseDuringPendingReadReleasesReader closes the stream while a Read
// is parked on the socket, and only then lets that Read see the upstream's
// end-of-body. Every step is ordered by a channel; the only clock involved is
// net/http's own post-close drain budget, which the terminator beats by
// microseconds.
//
// It reproduces the real hang only on Go 1.27.0 and 1.27.1, whose HTTP/1
// transport strands a Read that reaches end-of-body after a concurrent Close;
// it passes against the unfixed code on Go 1.26 and earlier, and should again
// once the toolchain carries the upstream fix (Go issue 81411).
// TestStreamCloseJoinsPendingReadBeforeClosingBody guards the ordering itself
// on every toolchain.
func TestStreamCloseDuringPendingReadReleasesReader(t *testing.T) {
	const first = "event: response.created\ndata: {}\n\n"
	release := make(chan struct{})
	terminated := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(brw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(first), first)
		_ = brw.Flush()
		<-release
		_, _ = brw.WriteString("0\r\n\r\n")
		_ = brw.Flush()
		close(terminated)
		// Hold the connection open so its only end-of-body is the terminator.
		_, _ = bufio.NewReader(conn).ReadByte()
	}))
	defer srv.Close()

	conns := make(chan *gatedConn, 1)
	ht := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		g := &gatedConn{Conn: c, entered: make(chan struct{}, 1), open: make(chan struct{}), closed: make(chan struct{})}
		conns <- g
		return g, nil
	}}
	// On failure the stranded Read is freed only when its pooled connection dies.
	defer ht.CloseIdleConnections()
	c := newClient(t, srv.URL, func(o *Options) { o.Transport = funcTransport{c: &http.Client{Transport: ht}} })

	resp, err := c.StreamResponse(context.Background(), testCred(), testRequest())
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	gc := <-conns

	buf := make([]byte, len(first))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read first event: %v", err)
	}
	gc.arm()

	readDone := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 64))
		readDone <- err
	}()
	wait := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	wait(gc.entered, "the body Read to reach the socket")
	close(release)
	wait(terminated, "the upstream to send its terminator")

	closed := make(chan struct{})
	go func() {
		_ = resp.Close()
		close(closed)
	}()
	// With the fix Close returns only once the parked Read has returned; at HEAD
	// it returns as soon as net/http has taken the early-close signal. Either way
	// the socket is now allowed to deliver the terminator.
	select {
	case <-closed:
	case <-gc.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return and did not tear down the connection")
	}
	close(gc.open)
	wait(closed, "Close to return")

	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("a body Read pending across Close never returned: the reader is stranded until the pooled connection idles out")
	}
}

// writeGate holds every socket Write issued after arm until release is closed,
// and reports the first such Write on entered. It stands in for an upstream
// that has stopped reading, so the transport's own writes block.
type writeGate struct {
	net.Conn
	mu        sync.Mutex
	armed     bool
	entered   chan struct{}
	enterOne  sync.Once
	release   chan struct{}
	releaseMu sync.Once
}

func newWriteGate(c net.Conn) *writeGate {
	return &writeGate{Conn: c, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *writeGate) arm() {
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
}

func (g *writeGate) open() { g.releaseMu.Do(func() { close(g.release) }) }

func (g *writeGate) Write(p []byte) (int, error) {
	g.mu.Lock()
	armed := g.armed
	g.mu.Unlock()
	if armed {
		g.enterOne.Do(func() { close(g.entered) })
		<-g.release
	}
	return g.Conn.Write(p)
}

// readEntered reports when the first Read reaches the wrapped body.
type readEntered struct {
	io.ReadCloser
	entered chan struct{}
	once    sync.Once
}

func (r *readEntered) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	return r.ReadCloser.Read(p)
}

// TestStreamCloseReleasesHTTP2ReadWhileResetIsBlocked covers the HTTP/2 side of
// the Close contract. When a request is cancelled, both HTTP/2 clients write
// RST_STREAM before they break the body's read pipe, so a peer that has stopped
// reading holds a pending Read until the write goes through. Closing the body
// breaks the pipe at once, and an HTTP/2 body is safe to close during a Read, so
// Close must go straight to it rather than first waiting for that Read.
func TestStreamCloseReleasesHTTP2ReadWhileResetIsBlocked(t *testing.T) {
	cases := []struct {
		name string
		rt   func(dial func(context.Context, string, string) (net.Conn, error)) http.RoundTripper
	}{
		{"x/net/http2", func(dial func(context.Context, string, string) (net.Conn, error)) http.RoundTripper {
			return &http2.Transport{DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
				raw, err := dial(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				tc := tls.Client(raw, cfg)
				if err := tc.HandshakeContext(ctx); err != nil {
					_ = raw.Close()
					return nil, err
				}
				return tc, nil
			}, TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}}}
		}},
		{"net/http", func(dial func(context.Context, string, string) (net.Conn, error)) http.RoundTripper {
			return &http.Transport{
				DialContext:       dial,
				ForceAttemptHTTP2: true,
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "x")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			srv.EnableHTTP2 = true
			srv.StartTLS()
			defer srv.Close()

			gates := make(chan *writeGate, 1)
			dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				g := newWriteGate(c)
				gates <- g
				return g, nil
			}
			rt := tc.rt(dial)
			c := newClient(t, srv.URL, func(o *Options) { o.Transport = funcTransport{c: &http.Client{Transport: rt}} })

			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			resp, err := c.StreamResponse(parent, testCred(), testRequest())
			if err != nil {
				t.Fatalf("StreamResponse: %v", err)
			}
			gate := <-gates
			// Unwind in order: let the reset through, then drop the connection.
			defer func() { _ = gate.Conn.Close() }()
			defer gate.open()

			sb := resp.Body.(*streamBody)
			if sb.proto != 2 {
				t.Fatalf("negotiated HTTP/%d, want HTTP/2", sb.proto)
			}
			if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil {
				t.Fatalf("read first byte: %v", err)
			}
			gate.arm()
			notify := &readEntered{ReadCloser: sb.rc, entered: make(chan struct{})}
			sb.rc = notify

			readDone := make(chan struct{})
			go func() {
				_, _ = resp.Body.Read(make([]byte, 1))
				close(readDone)
			}()
			wait := func(ch <-chan struct{}, what string) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(2 * time.Second):
					t.Fatalf("timed out waiting for %s", what)
				}
			}
			wait(notify.entered, "the body Read to start")
			cancelParent() // the client went away: the transport starts its reset
			wait(gate.entered, "the transport to start writing RST_STREAM")

			closeDone := make(chan struct{})
			go func() {
				_ = resp.Close()
				close(closeDone)
			}()
			wait(closeDone, "Close to return while the reset write is blocked")
			wait(readDone, "the pending Read to return")
		})
	}
}
