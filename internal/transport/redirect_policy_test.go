package transport

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

// redirectRT answers in memory and never dials: hop 0 gets the armed 3xx, and
// any later hop is a followed redirect.
type redirectRT struct {
	mu                   sync.Mutex
	status, hops         int
	location, auth, body string
}

func (r *redirectRT) arm(status int, location string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.location, r.hops, r.auth, r.body = status, location, 0, "", ""
}

func (r *redirectRT) RoundTrip(req *http.Request) (*http.Response, error) {
	var b []byte
	if req.Body != nil { // a followed 303 drops the body
		var err error
		if b, err = io.ReadAll(req.Body); err != nil {
			return nil, err
		}
		_ = req.Body.Close()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hops++
	status, header := http.StatusOK, http.Header{} // a followed hop ends the chain
	if r.hops == 1 {
		r.auth, r.body = req.Header.Get("Authorization"), string(b)
		status, header = r.status, http.Header{"Location": {r.location}}
	}
	return &http.Response{
		StatusCode: status, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: header, Body: io.NopCloser(strings.NewReader("3xx-body")), Request: req,
	}, nil
}

type stubTransport struct{ client *http.Client }

func (s stubTransport) Client() *http.Client { return s.client }
func (s stubTransport) Kind() string         { return KindStd }

// FuzzProductionRedirectPolicy sends a static POST through every production
// client (std, utls, auto before and after its switch) and varies only the 3xx
// status and Location upstream answers with. The credential and body must make
// exactly one hop, and the caller must get that 3xx back as sent; net/http may
// instead error on a Location it cannot parse, but still without a second hop.
func FuzzProductionRedirectPolicy(f *testing.F) {
	const (
		start = "https://upstream.redirect-canary.invalid/v1/responses"
		auth  = "Bearer redirect-canary-token"
		body  = `{"model":"gpt-5","stream":true}`
	)
	for _, loc := range []string{"/target", "https://canary.redirect-canary.invalid/steal",
		"//canary.redirect-canary.invalid/x", "http://upstream.redirect-canary.invalid/", "https://[::1", ""} {
		for _, status := range []uint8{1, 2, 3, 7, 8} {
			f.Add(status, loc)
		}
	}

	rec := &redirectRT{}
	clients := map[string]*http.Client{}
	for name, tr := range map[string]Transport{"std": NewStd(DefaultOptions()), "utls": NewUTLS(DefaultOptions())} {
		c := *tr.Client() // the production client, with only its RoundTripper swapped
		c.Transport = rec
		clients[name] = &c
	}
	for _, name := range []string{"auto/std", "auto/utls"} {
		auto := NewAuto(DefaultOptions(), discardLogger()).(*autoTransport)
		// The halves keep net/http's follow-everything default: only auto's own policy may hold.
		auto.std, auto.utls = stubTransport{&http.Client{Transport: rec}}, stubTransport{&http.Client{Transport: rec}}
		if name == "auto/utls" {
			auto.ReportGate()
		}
		clients[name] = auto.Client()
	}

	f.Fuzz(func(t *testing.T, rawStatus uint8, location string) {
		status := 300 + int(rawStatus)%100
		for name, c := range clients {
			rec.arm(status, location)
			req, _ := http.NewRequest(http.MethodPost, start, strings.NewReader(body))
			req.Header.Set("Authorization", auth)
			resp, err := c.Do(req)
			if rec.hops != 1 || rec.auth != auth || rec.body != body {
				t.Fatalf("%s %d %q: hops=%d auth=%q body=%q, want one hop with the original request", name, status, location, rec.hops, rec.auth, rec.body)
			}
			if err != nil {
				if _, perr := req.URL.Parse(location); perr == nil {
					t.Fatalf("%s %d %q: Do: %v", name, status, location, err)
				}
				continue
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != status || !slices.Equal(resp.Header["Location"], []string{location}) || string(got) != "3xx-body" {
				t.Fatalf("%s: got %d %q %q, want %d %q verbatim", name, resp.StatusCode, resp.Header["Location"], got, status, location)
			}
		}
	})
}
