package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/effort"
	"github.com/hughescr/utraque/internal/proxyhdr"
	"github.com/hughescr/utraque/internal/router"
)

// Separate namespaces let the oracle distinguish the origin of a value: a
// header-only secret is forbidden in the upstream body, but user text that
// looks like an Authorization field is still user text, not a credential.
const (
	callerMark = "UTRAQUE-FUZZ-CALLER-"
	localMark  = "UTRAQUE-FUZZ-LOOPBACK"
	deepKey    = "UTRAQUE-FUZZ-DEEPSEEK"
	accountID  = "UTRAQUE-FUZZ-CODEX-ACCOUNT"
	refreshKey = "UTRAQUE-FUZZ-CODEX-REFRESH"
	bodyMark   = "UTRAQUE-FUZZ-BODY"
)

// credentialHeaders are credential-bearing request headers beyond the four
// (Authorization, X-Api-Key, Cookie, the local token) that request always sets.
// Every request carries each one with its own caller canary, so an allowlist
// that gains one of them, or a prefix rule that forwards a whole family, shows
// up on the seed corpus. They are never forwarded to DeepSeek (an allowlist of
// Accept, Anthropic-Beta, Anthropic-Version, Content-Type, User-Agent and
// X-Request-Id; internal/deepseek.requestHeaderAllow) nor to Codex (which
// builds its headers from scratch and copies none). The Anthropic passthrough
// documents the opposite default, "relays every caller header except the
// connection-scoped set" and the X-Utraque-* namespace, so each is forwarded
// verbatim except Proxy-Authorization, which is hop-by-hop (RFC 7230 6.1; its
// hopByHop table lists it).
var credentialHeaders = []string{
	"Proxy-Authorization", "Anthropic-Auth-Token", "Openai-Api-Key", "Api-Key",
	"Chatgpt-Account-Id", "Access-Token", "Refresh-Token", "Id-Token", "Set-Cookie",
	"X-Anthropic-Custom-Credential",
}

func headerCanary(name string) string { return callerMark + "hdr-" + strings.ToLower(name) }

// hopByHopHeader is the one entry of credentialHeaders the Anthropic
// passthrough must withhold as well.
const hopByHopHeader = "Proxy-Authorization"

type credentialHit struct {
	backend router.Backend
	uri     string
	header  http.Header
	body    string
}

type credentialHarness struct {
	front http.Handler
	token string
	mu    sync.Mutex
	hits  []credentialHit
}

// Drive main's actual dispatch and three real HTTP clients. Each provider has
// its own loopback origin. No HOME lookup, live catalog, or auth refresh.
func newCredentialHarness(f *testing.F) *credentialHarness {
	f.Helper()
	previous := router.DefaultRegistry
	router.DefaultRegistry = router.NewStaticRegistry()
	f.Cleanup(func() { router.DefaultRegistry = previous })
	h := &credentialHarness{token: jwtWithExp(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))}
	upstream := func(backend router.Backend) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "cannot read test request", http.StatusInternalServerError)
				return
			}
			// net/http represents Host separately, and populates Trailer only
			// after EOF. Include both in the observed upstream header set.
			headers := r.Header.Clone()
			headers.Set("Host", r.Host)
			for k, vs := range r.Trailer {
				headers[k] = append(headers[k], vs...)
			}
			h.mu.Lock()
			h.hits = append(h.hits, credentialHit{backend, r.RequestURI, headers, string(b)})
			h.mu.Unlock()
			// Reject the catalog request so each Codex input exercises a cold
			// fetch without replacing the registry or scheduling a revalidation.
			if r.URL.Path == codexBasePath+"/models" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("X-Utraque-Future", "upstream-only")
			w.Header().Add("Connection", "keep-alive, x-REQUEST-Id")
			w.Header().Set("X-Request-Id", "upstream-hop-only")
			if backend == router.BackendCodex {
				writeSSE(w, solStream)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		}))
		f.Cleanup(s.Close)
		return s.URL
	}
	cfg := config.Default()
	cfg.LocalToken = localMark
	cfg.Anthropic.BaseURL = upstream(router.BackendAnthropic)
	cfg.DeepSeek.BaseURL, cfg.DeepSeek.APIKey = upstream(router.BackendDeepSeek), deepKey
	cfg.Codex.BaseURL = upstream(router.BackendCodex) + codexBasePath
	cfg.Codex.TokenURL = cfg.Codex.BaseURL + "/unexpected-refresh"
	cfg.Codex.AuthFile = filepath.Join(f.TempDir(), "auth.json")
	fixture := fmt.Sprintf(`{"tokens":{"access_token":%q,"account_id":%q,"refresh_token":%q}}`, h.token, accountID, refreshKey)
	if err := os.WriteFile(cfg.Codex.AuthFile, []byte(fixture), 0o600); err != nil {
		f.Fatal(err)
	}
	s, err := newServer(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		f.Fatal(err)
	}
	h.front = s
	return h
}

// take returns, and clears, the upstream requests one front-end call caused.
//
// Hits are appended by the loopback handlers, on their own goroutines. A Codex
// call always makes one catalog lookup, and the leg abandons that lookup after
// its 2s catalog timeout (internal/codex/leg.DefaultCatalogTimeout) and goes on
// to the inference. If the loopback handler had not yet run when the client gave
// up (a loaded host, a descheduled goroutine), its hit would be appended after
// the swap below and blamed on the next input ("request reached codex, want only
// anthropic"). Waiting for the lookup's own hit closes that window; the bound is
// generous because a call that never reached the server (and so never records)
// must not hang the run.
func (h *credentialHarness) take(want router.Backend) []credentialHit {
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.mu.Lock()
		catalog := slices.ContainsFunc(h.hits, func(hit credentialHit) bool {
			return hit.backend == router.BackendCodex && strings.HasSuffix(strings.SplitN(hit.uri, "?", 2)[0], "/models")
		})
		if want != router.BackendCodex || catalog || time.Now().After(deadline) {
			hits := h.hits
			h.hits = nil
			h.mu.Unlock()
			return hits
		}
		h.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *credentialHarness) request(t *testing.T, model, name, value, field string, want router.Backend) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": model, "max_tokens": 32,
		"messages":         []map[string]string{{"role": "user", "content": bodyMark + ": " + value}},
		"client_extension": map[string]string{field: value},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	r.Host = "localhost" // An accepted Host; the "host" guard case below overrides it.
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+callerMark+"oauth")
	r.Header.Set("X-Api-Key", callerMark+"key")
	r.Header.Add("Cookie", "session="+callerMark+"cookie")
	r.Header.Set(proxyhdr.LocalToken, localMark)
	for _, n := range credentialHeaders {
		r.Header.Set(n, headerCanary(n))
	}
	if name != "" && want == router.BackendAnthropic {
		// Unlike the other legs, passthrough must preserve repeated bearer
		// values but strip even a noncanonical spelling of the local token.
		r.Header.Add("aUtHoRiZaTiOn", "Bearer "+callerMark+strconv.QuoteToASCII(value))
		spelling := strings.ToLower(proxyhdr.LocalToken)
		if len(name) == len(proxyhdr.LocalToken) && strings.EqualFold(name, proxyhdr.LocalToken) {
			spelling = name
		}
		r.Header[spelling] = append(r.Header[spelling], localMark+"-duplicate")
	} else if name != "" {
		// Add canonicalizes (as net/http does on receipt). Also exercise a raw
		// noncanonical map key, which another middleware could supply. Keep
		// arbitrary values wire-safe in potentially allowlisted headers; raw
		// CR/LF goes in the denied duplicate, where it must never be copied.
		v := strconv.QuoteToASCII(value)
		switch strings.ToLower(name) {
		case "accept", "anthropic-beta", "anthropic-version", "content-type", "user-agent", "x-request-id":
			// DeepSeek intentionally forwards ordinary values in these fields.
		default:
			v = callerMark + "extra-" + v
		}
		r.Header.Add(name, v)
		r.Header[name] = append(r.Header[name], v)
		r.Header.Add("aUtHoRiZaTiOn", "Bearer "+callerMark+"duplicate-"+value)
		r.Header.Add("x-API-kEy", callerMark+"second-key")
		r.Header.Add("COOKIE", "other="+callerMark+"second-cookie")
		r.Header.Add(proxyhdr.LocalToken, localMark+"-duplicate")
	}
	if name != "" {
		r.Header.Set("X-Utraque-Future", "caller-only")
		if strings.HasPrefix(strings.ToLower(name), "x-utraque-") && !strings.EqualFold(name, proxyhdr.LocalToken) {
			r.Header[name] = []string{"caller-only"}
		}
		r.Header.Set("X-Request-Id", "caller-hop-only")
		r.Header.Add("Connection", " , x-REQUEST-Id, ")
		if strings.EqualFold(name, "Connection") {
			// Fuzz the token list but restrict nominations to a harmless test
			// field, so unrelated credential assertions stay meaningful.
			for tok := range strings.SplitSeq(value, ",") {
				if strings.EqualFold(strings.TrimSpace(tok), "Anthropic-Version") {
					r.Header.Set("Anthropic-Version", "nominated")
					r.Header.Add("Connection", tok)
				}
			}
		}
	}
	guarded := true
	switch strings.ToLower(name) {
	case "origin", "sec-fetch-site":
		r.Header.Set(name, "foreign.invalid")
	case "host":
		// A real net/http server moves Host out of Header, and supplies the
		// local listener address separately from the peer address.
		r.Host = "foreign.invalid"
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1).To4()}))
	default:
		guarded = false
	}
	wantBearer := slices.Clone(r.Header.Values("Authorization"))
	w := httptest.NewRecorder()
	h.front.ServeHTTP(w, r)
	if guarded {
		if hits := h.take(""); w.Code != http.StatusForbidden || len(hits) != 0 {
			t.Fatalf("browser guard: HTTP %d, %d upstream calls; want 403, zero calls", w.Code, len(hits))
		}
		return
	}
	hits := h.take(want)
	if want == "" {
		if len(hits) != 0 || w.Code != http.StatusNotFound {
			t.Fatalf("unknown model: %d upstream calls, HTTP %d", len(hits), w.Code)
		}
		return
	}
	if len(w.Header().Values("X-Utraque-Future")) != 0 || w.Header().Get("X-Request-Id") == "upstream-hop-only" {
		t.Fatalf("%s relayed a reserved or nominated upstream response header", want)
	}
	inference := 0
	for _, hit := range hits {
		if hit.backend != want {
			t.Fatalf("request reached %s, want only %s", hit.backend, want)
		}
		path, _, _ := strings.Cut(hit.uri, "?")
		inferencePath := "/v1/messages"
		if want == router.BackendCodex {
			inferencePath = codexBasePath + "/responses"
		}
		if path == inferencePath {
			inference++
			if !strings.Contains(hit.body, bodyMark) {
				t.Fatalf("test message did not reach %s", want)
			}
		} else if want != router.BackendCodex || path != codexBasePath+"/models" {
			t.Fatalf("unexpected %s request to %q", want, hit.uri)
		}
		// Provider-owned secrets belong only in their dedicated fields. Remove
		// exactly the fields validated below before scanning the remainder.
		deny := []string{localMark, refreshKey, accountID, h.token, deepKey}
		switch want {
		case router.BackendAnthropic:
			if !slices.Equal(hit.header.Values("Authorization"), wantBearer) {
				t.Fatal("Anthropic did not receive the caller's exact bearer")
			}
			// Passthrough relays caller headers verbatim, except the hop-by-hop
			// set, which includes Proxy-Authorization: that one is meant for
			// the proxy hop, and utraque is that hop.
			for _, n := range credentialHeaders {
				got := hit.header.Values(n)
				if n == hopByHopHeader {
					if len(got) != 0 {
						t.Fatalf("Anthropic received hop-by-hop %s", n)
					}
				} else if !slices.Equal(got, []string{headerCanary(n)}) {
					t.Fatalf("Anthropic passthrough did not relay %s verbatim", n)
				}
			}
		case router.BackendCodex:
			if !slices.Equal(hit.header.Values("Authorization"), []string{"Bearer " + h.token}) || !slices.Equal(hit.header.Values("Chatgpt-Account-Id"), []string{accountID}) || len(hit.header.Values("X-Api-Key")) != 0 {
				t.Fatal("incorrect Codex credentials")
			}
			hit.header.Del("Authorization")
			hit.header.Del("Chatgpt-Account-Id")
			deny = append(deny, callerMark)
		case router.BackendDeepSeek:
			if !slices.Equal(hit.header.Values("X-Api-Key"), []string{deepKey}) || len(hit.header.Values("Authorization")) != 0 {
				t.Fatal("incorrect DeepSeek credentials")
			}
			hit.header.Del("X-Api-Key")
			deny = append(deny, callerMark)
		}
		for k := range hit.header {
			if strings.HasPrefix(strings.ToLower(k), "x-utraque-") {
				t.Fatalf("reserved header %s reached %s", k, want)
			}
		}
		if hit.header.Get("X-Request-Id") == "caller-hop-only" || hit.header.Get("Anthropic-Version") == "nominated" {
			t.Fatalf("Connection-nominated caller field reached %s", want)
		}
		if want != router.BackendAnthropic && len(hit.header.Values("Cookie")) != 0 {
			t.Fatalf("cookie header reached %s", want)
		}
		// Neither owner-credentialed backend relays any caller credential header,
		// under any spelling. (Codex's own Authorization and Chatgpt-Account-Id
		// were validated and removed above.)
		if want != router.BackendAnthropic {
			for _, n := range credentialHeaders {
				if len(hit.header.Values(n)) != 0 {
					t.Fatalf("caller header %s reached %s", n, want)
				}
			}
			for k := range hit.header {
				if strings.HasPrefix(strings.ToLower(k), "x-anthropic-") {
					t.Fatalf("caller X-Anthropic-* header %s reached %s", k, want)
				}
			}
		}
		for _, secret := range deny {
			if strings.Contains(hit.body, secret) || strings.Contains(hit.uri, secret) {
				t.Fatalf("forbidden canary reached %s in URI or body", want)
			}
			for k, vs := range hit.header {
				if strings.Contains(k, secret) || strings.Contains(strings.Join(vs, "\n"), secret) {
					t.Fatalf("forbidden canary reached %s in header %q", want, k)
				}
			}
		}
	}
	if inference != 1 || w.Code != http.StatusOK {
		t.Fatalf("want one %s inference; got %d, HTTP %d: %.250s", want, inference, w.Code, w.Body.String())
	}
}

func FuzzInferenceCredentialIsolation(f *testing.F) {
	h := newCredentialHarness(f)
	for _, seed := range [][3]string{
		{"Origin", "foreign", "test"},
		{"Sec-Fetch-Site", "cross-site", "test"},
		{"Host", "foreign.invalid", "test"},
		{"authorization", "Bearer only-in-prompt", "Authorization"},
		{"x-aPi-kEY", "a\r\nAuthorization: Bearer synthetic", "headers"},
		{"coOKIE", "session=one; other=two", "cookie"},
		{strings.ToLower(proxyhdr.LocalToken), "local-token", "metadata"},
		{"Anthropic-Beta", "realistic-beta", "β"},
		{"X-Unrecognised", "duplicate", "x-api-key"},
		{"x-uTrAqUe-Unlisted", "extension", "note"},
		{"Connection", "unknown, \tANTHROPIC-version ,", "headers"},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	// The fuzzed name slot must also be able to spell a real credential header
	// (blind bytes almost never do), in lower, canonical and upper case.
	for _, n := range credentialHeaders {
		f.Add(strings.ToLower(n), "opaque-value", "note")
		f.Add(n, "Bearer synthetic", "headers")
		f.Add(strings.ToUpper(n), "a\r\nb: c", "client_extension")
	}
	f.Add("X-Anthropic-Unlisted", "x", "metadata")
	f.Add("x-anthropic-beta-extra", "x", "metadata")
	f.Fuzz(func(t *testing.T, name, value, field string) {
		// Do not mistake a fuzzer copying the fixture literal into user data
		// for the proxy transferring it from the credential header.
		for _, s := range []string{name, value, field} {
			if len(s) > 4096 || strings.Contains(s, "UTRAQUE-FUZZ-") || strings.Contains(s, h.token) {
				t.Skip()
			}
		}
		h.request(t, "deepseek-flash", name, value, field, router.BackendDeepSeek)
		h.request(t, "gpt-6.2-sol", name, value, field, router.BackendCodex)
		h.request(t, "claude-sonnet", name, value, field, router.BackendAnthropic)
	})
}

func FuzzModelCredentialDestination(f *testing.F) {
	// A fixed registry is part of the property: aliases can legitimately
	// change after a catalog refresh or picker publication in a live process.
	h := newCredentialHarness(f)
	reg := router.NewStaticRegistry()
	for _, s := range []string{"", "not-a-model", "claude-sonnet", "opus[1m]", "sol-high", "gpt-future-slug", " gPt-6.2-sol\t", "deepseek-flash", "deepseek-v4-pro", "deepseek-v4-flash-vision-exp", "deepseek-flash-extra", " anthropic-compat.DEEPSEEK-V4-FLASH\n", "anthropic-compat.sol", "anthropic-compat.unknown", "ｇｐｔ-6", "xgpt-6", "gpt-//elsewhere.invalid"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, model string) {
		if len(model) > 4096 || strings.Contains(model, "UTRAQUE-FUZZ-") || strings.Contains(model, h.token) {
			t.Skip()
		}
		// encoding/json transmits invalid UTF-8 as replacement characters.
		model = string([]rune(model))
		lower := strings.ToLower(strings.TrimSpace(model))
		base, compat := strings.CutPrefix(lower, "anthropic-compat.")
		var want router.Backend
		switch {
		case !compat && (strings.HasPrefix(base, "claude") || strings.HasPrefix(base, "anthropic") || strings.HasPrefix(base, "sonnet") || strings.HasPrefix(base, "opus") || strings.HasPrefix(base, "haiku") || strings.HasPrefix(base, "fable")):
			want = router.BackendAnthropic
		case slices.Contains([]string{"deepseek-flash", "deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp"}, base):
			want = router.BackendDeepSeek
		case strings.HasPrefix(base, "gpt-"):
			want = router.BackendCodex
		default:
			alias := base
			if i := strings.LastIndexByte(base, '-'); i >= 0 && effort.Known(effort.Level(base[i+1:])) {
				alias = base[:i]
			}
			_, exact := reg.Resolve(base)
			if _, suffixed := reg.Resolve(alias); exact || suffixed {
				want = router.BackendCodex
			}
		}
		// Compare a direct decision with the subsequent independent resolution
		// inside the dispatcher, and both with the expected destination.
		dec, err := router.Resolve(model, "")
		if dec.Backend != want || (err != nil) != (want == "") {
			t.Fatalf("model %q: direct decision %q, error %v; want %q", model, dec.Backend, err, want)
		}
		h.request(t, model, "", "hello", "note", want)
	})
}
