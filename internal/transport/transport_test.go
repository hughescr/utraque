package transport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultOptions(t *testing.T) {
	o := DefaultOptions()
	if o.DialTimeout != 10*time.Second {
		t.Errorf("DialTimeout = %v, want 10s", o.DialTimeout)
	}
	if o.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 10s", o.TLSHandshakeTimeout)
	}
	if o.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 120s", o.ResponseHeaderTimeout)
	}
	if o.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", o.IdleConnTimeout)
	}
	if o.MaxIdleConns != 64 || o.MaxIdleConnsPerHost != 16 {
		t.Errorf("conn caps = %d/%d, want 64/16", o.MaxIdleConns, o.MaxIdleConnsPerHost)
	}
	if !o.DisableCompression {
		t.Error("DisableCompression = false, want true for passthrough fidelity")
	}
}

func TestNewStdShape(t *testing.T) {
	tr := NewStd(DefaultOptions())
	if tr.Kind() != KindStd {
		t.Errorf("Kind() = %q, want %q", tr.Kind(), KindStd)
	}
	c := tr.Client()
	if c == nil {
		t.Fatal("Client() = nil")
	}
	if c != tr.Client() {
		t.Error("Client() returned a different client on the second call; the pool must be shared")
	}
	if c.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, want 0 (an overall deadline would cut SSE streams)", c.Timeout)
	}
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect = nil, want the no-redirect policy")
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Errorf("CheckRedirect returned %v, want http.ErrUseLastResponse", err)
	}
}

func TestNewStdNormalizesZeroOptions(t *testing.T) {
	tr := NewStd(Options{})
	ht, ok := tr.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", tr.Client().Transport)
	}
	d := DefaultOptions()
	if ht.TLSHandshakeTimeout != d.TLSHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %v, want %v", ht.TLSHandshakeTimeout, d.TLSHandshakeTimeout)
	}
	if ht.ResponseHeaderTimeout != d.ResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", ht.ResponseHeaderTimeout, d.ResponseHeaderTimeout)
	}
	if ht.IdleConnTimeout != d.IdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", ht.IdleConnTimeout, d.IdleConnTimeout)
	}
	if ht.MaxIdleConns != d.MaxIdleConns || ht.MaxIdleConnsPerHost != d.MaxIdleConnsPerHost {
		t.Errorf("conn caps = %d/%d, want %d/%d",
			ht.MaxIdleConns, ht.MaxIdleConnsPerHost, d.MaxIdleConns, d.MaxIdleConnsPerHost)
	}
	if !ht.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want true")
	}
}

func TestNewStdHonoursExplicitOptions(t *testing.T) {
	tr := NewStd(Options{
		DialTimeout:           time.Second,
		TLSHandshakeTimeout:   2 * time.Second,
		ResponseHeaderTimeout: 3 * time.Second,
		IdleConnTimeout:       4 * time.Second,
		MaxIdleConns:          5,
		MaxIdleConnsPerHost:   6,
		DisableCompression:    false,
	})
	ht := tr.Client().Transport.(*http.Transport)
	if ht.ResponseHeaderTimeout != 3*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 3s", ht.ResponseHeaderTimeout)
	}
	if ht.MaxIdleConns != 5 || ht.MaxIdleConnsPerHost != 6 {
		t.Errorf("conn caps = %d/%d, want 5/6", ht.MaxIdleConns, ht.MaxIdleConnsPerHost)
	}
	if ht.DisableCompression {
		t.Error("DisableCompression = true, want the explicit false")
	}
}

func TestNewStdClientDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		io.WriteString(w, "followed")
	})
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := NewStd(DefaultOptions()).Client().Get(srv.URL + "/start")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/target" {
		t.Errorf("Location = %q, want /target", got)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target fetched %d time(s), want 0", n)
	}
}
