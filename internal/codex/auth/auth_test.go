//go:build unix

// These tests never touch the real ~/.codex/auth.json and never contact the
// real auth.openai.com. Every Source is built by newTestSource, which asserts
// its path is inside the test's TempDir and points its TokenURL at an
// httptest fake. Writing the real file could log the user out of their own
// Codex CLI, so that isolation is load-bearing, not incidental.
package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/apierr"
)

// ---- fakes & helpers -------------------------------------------------------

// fakeOAuth is a stand-in for auth.openai.com/oauth/token. It records how many
// refresh calls it received and the refresh_token last presented, and returns
// configurable rotated tokens. A non-nil gate blocks each handler until closed,
// which lets a test hold a refresh in-flight.
type fakeOAuth struct {
	srv *httptest.Server

	mu               sync.Mutex
	lastRefreshToken string
	lastScope        string
	scopePresent     bool

	calls atomic.Int64

	// knobs (read once per request under mu)
	status        int       // 0 => 200
	newAccessExp  time.Time // exp baked into the returned access-token JWT
	newIDToken    string
	newRefreshTok string
	gate          chan struct{}
	failBadJSON   bool
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	t.Helper()
	f := &fakeOAuth{
		newAccessExp:  time.Now().Add(2 * time.Hour),
		newIDToken:    "id-token-value-should-never-be-logged",
		newRefreshTok: "rotated-refresh-token",
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOAuth) handle(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)

	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		ClientID     string `json:"client_id"`
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
	}
	_ = json.Unmarshal(body, &req)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(body, &raw)

	f.mu.Lock()
	f.lastRefreshToken = req.RefreshToken
	f.lastScope = req.Scope
	_, f.scopePresent = raw["scope"]
	status := f.status
	exp := f.newAccessExp
	idTok := f.newIDToken
	refTok := f.newRefreshTok
	gate := f.gate
	badJSON := f.failBadJSON
	f.mu.Unlock()

	if gate != nil {
		<-gate
	}
	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh token expired"}`)
		return
	}
	if badJSON {
		_, _ = io.WriteString(w, `{not json`)
		return
	}
	resp := map[string]string{
		"access_token":  makeJWT(exp),
		"id_token":      idTok,
		"refresh_token": refTok,
		"token_type":    "Bearer",
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// slogTextLogger builds a debug-level text logger over w so a test can scan
// everything the Source logged for leaked secrets.
func slogTextLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// makeJWT builds an unsigned JWT whose payload carries the given exp. No
// signature is produced or checked; utraque reads exp only.
func makeJWT(exp time.Time) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(`{"exp":%d,"sub":"codex-user"}`, exp.Unix())))
	return hdr + "." + payload + ".unsigned"
}

// authJSON is a builder for a fake auth.json body.
type authJSON struct {
	openAIAPIKey string
	lastRefresh  string
	access       string
	idToken      string
	refresh      string
	accountID    string
	unknownTop   json.RawMessage // extra top-level key "unknown_top"
	unknownTok   json.RawMessage // extra tokens sub-key "unknown_tok"
	omitAccount  bool
}

func (a authJSON) bytes() []byte {
	tokens := map[string]json.RawMessage{}
	put := func(m map[string]json.RawMessage, k, v string) {
		if v != "" {
			raw, _ := json.Marshal(v)
			m[k] = raw
		}
	}
	put(tokens, keyAccessToken, a.access)
	put(tokens, keyIDToken, a.idToken)
	put(tokens, keyRefreshToken, a.refresh)
	if !a.omitAccount {
		put(tokens, keyAccountID, a.accountID)
	}
	if len(a.unknownTok) > 0 {
		tokens["unknown_tok"] = a.unknownTok
	}

	top := map[string]json.RawMessage{}
	put(top, "OPENAI_API_KEY", a.openAIAPIKey)
	put(top, keyLastRefresh, a.lastRefresh)
	if len(a.unknownTop) > 0 {
		top["unknown_top"] = a.unknownTop
	}
	tokRaw, _ := json.Marshal(tokens)
	top[keyTokens] = tokRaw

	b, _ := json.Marshal(top)
	return b
}

// newTestSource writes body to a fresh temp auth.json and returns a Source
// wired to a fake OAuth endpoint. It fails the test if the path is not inside
// the temp dir — the guardrail against ever operating on the real file.
func newTestSource(t *testing.T, f *fakeOAuth, body []byte) (*Source, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if !strings.HasPrefix(path, dir) {
		t.Fatalf("refusing to run: auth path %q is not under the temp dir %q", path, dir)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write fake auth.json: %v", err)
	}
	s, err := New(Options{
		Path:        path,
		TokenURL:    f.srv.URL, // never the real endpoint
		ClientID:    "app_TEST",
		RefreshSkew: 120 * time.Second,
		LockTimeout: 2 * time.Second,
		HTTPClient:  f.srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, path
}

func readTokens(t *testing.T, path string) (top, tokens map[string]json.RawMessage) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	top = map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("parse back: %v", err)
	}
	tokens = map[string]json.RawMessage{}
	if raw, ok := top[keyTokens]; ok {
		if err := json.Unmarshal(raw, &tokens); err != nil {
			t.Fatalf("parse tokens back: %v", err)
		}
	}
	return top, tokens
}

func rawString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("expected JSON string, got %s", raw)
	}
	return s
}

// ---- expiry / JWT ----------------------------------------------------------

func TestJWTExp(t *testing.T) {
	want := time.Now().Add(90 * time.Minute).Truncate(time.Second)
	exp, ok := jwtExp(makeJWT(want))
	if !ok {
		t.Fatal("jwtExp reported not-ok for a valid token")
	}
	if !exp.Equal(want) {
		t.Errorf("exp = %s, want %s", exp, want)
	}
}

func TestJWTExpUnparseable(t *testing.T) {
	for _, tok := range []string{"", "no-dots", "only.two", "a.!!!notbase64!!!.c"} {
		if _, ok := jwtExp(tok); ok {
			t.Errorf("jwtExp(%q) = ok, want not-ok", tok)
		}
	}
}

func TestAccessTokenExpiryFallsBackToLastRefresh(t *testing.T) {
	last := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	exp, ok := accessTokenExpiry("not-a-jwt", last.Format(time.RFC3339), fallbackTTL)
	if !ok {
		t.Fatal("expected fallback to succeed")
	}
	if want := last.Add(fallbackTTL); !exp.Equal(want) {
		t.Errorf("fallback exp = %s, want %s", exp, want)
	}
}

func TestAccessTokenExpiryNoneAvailable(t *testing.T) {
	if _, ok := accessTokenExpiry("not-a-jwt", "also-not-a-time", fallbackTTL); ok {
		t.Error("expected not-ok when neither JWT nor last_refresh is usable")
	}
}

// ---- happy path & caching --------------------------------------------------

func TestGetFreshTokenNoRefresh(t *testing.T) {
	f := newFakeOAuth(t)
	s, _ := newTestSource(t, f, authJSON{
		access:    makeJWT(time.Now().Add(1 * time.Hour)),
		refresh:   "refresh-A",
		accountID: "acct-123",
	}.bytes())

	cred, err := s.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cred.AccountID != "acct-123" {
		t.Errorf("AccountID = %q", cred.AccountID)
	}
	if cred.AccessToken == "" || cred.Exp.Before(time.Now()) {
		t.Errorf("credential looks invalid: exp=%s", cred.Exp)
	}
	// A second Get is served from cache; still no network.
	if _, err := s.Get(context.Background()); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("fresh token should not refresh; server calls = %d", n)
	}
}

func TestGetRefreshesStaleToken(t *testing.T) {
	f := newFakeOAuth(t)
	s, path := newTestSource(t, f, authJSON{
		access:    makeJWT(time.Now().Add(30 * time.Second)), // within skew => stale
		refresh:   "refresh-old",
		accountID: "acct-9",
	}.bytes())

	cred, err := s.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("expected exactly one refresh, got %d", n)
	}
	if !cred.Exp.After(time.Now().Add(time.Hour)) {
		t.Errorf("refreshed exp not in the future: %s", cred.Exp)
	}
	// The refreshed access token was written back.
	_, tokens := readTokens(t, path)
	if got := rawString(t, tokens[keyAccessToken]); got != cred.AccessToken {
		t.Error("written access token does not match returned credential")
	}
	// The refresh request must match the Codex CLI's own body, which sends no
	// scope; adding one risks a downscoped or rejected grant.
	f.mu.Lock()
	scopePresent := f.scopePresent
	f.mu.Unlock()
	if scopePresent {
		t.Error("refresh request included a scope field; the Codex CLI sends none")
	}
}

// TestRefreshDoesNotClobberConcurrentExternalWrite proves the write-back guard:
// a writer that does NOT honour our advisory lock (a Codex CLI that just
// truncates auth.json) can rewrite the file while our refresh is mid-network.
// We must detect the change before our rename and adopt the external write
// rather than clobber it with our own rotated token.
func TestRefreshDoesNotClobberConcurrentExternalWrite(t *testing.T) {
	f := newFakeOAuth(t)
	f.gate = make(chan struct{}) // hold the refresh in-flight
	s, path := newTestSource(t, f, authJSON{
		access:    makeJWT(time.Now().Add(5 * time.Second)), // stale => refresh
		refresh:   "refresh-old",
		accountID: "acct-clobber",
	}.bytes())

	type result struct {
		cred Credential
		err  error
	}
	done := make(chan result, 1)
	go func() {
		c, e := s.Get(context.Background())
		done <- result{c, e}
	}()

	// Wait until the leader is inside the network refresh (holding the lock).
	deadline := time.Now().Add(2 * time.Second)
	for f.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if f.calls.Load() == 0 {
		t.Fatal("refresh never reached the server")
	}

	// A non-cooperating external writer replaces auth.json with a fresh token.
	// A longer refresh token guarantees a different file size, so the change is
	// detected regardless of mtime resolution.
	extAccess := makeJWT(time.Now().Add(3 * time.Hour))
	ext := authJSON{
		access:     extAccess,
		refresh:    "refresh-external-and-noticeably-longer-than-the-original",
		accountID:  "acct-clobber",
		unknownTop: json.RawMessage(`{"written_by":"a-peer-that-ignores-our-lock"}`),
	}.bytes()
	if err := os.WriteFile(path, ext, 0o600); err != nil {
		t.Fatal(err)
	}

	close(f.gate) // let our refresh complete
	res := <-done
	if res.err != nil {
		t.Fatalf("Get: %v", res.err)
	}

	// The external write must survive on disk (not clobbered by our refresh),
	// and we must have adopted it as our credential.
	_, tokens := readTokens(t, path)
	if got := rawString(t, tokens[keyAccessToken]); got != extAccess {
		t.Errorf("external write was clobbered: on-disk access token = %q, want the external one", got)
	}
	if res.cred.AccessToken != extAccess {
		t.Errorf("Get returned %q, want the adopted external token", res.cred.AccessToken)
	}
}

// TestRefreshSurvivesPersistFailure proves that when the refresh itself
// succeeds but persistence fails, the freshly rotated credential is kept in
// memory and served rather than discarded — discarding it would strand us with
// a now-dead refresh token on disk and force a re-login.
func TestRefreshSurvivesPersistFailure(t *testing.T) {
	f := newFakeOAuth(t)
	s, path := newTestSource(t, f, authJSON{
		access:    makeJWT(time.Now().Add(5 * time.Second)), // stale => refresh
		refresh:   "refresh-old",
		accountID: "acct-persist",
	}.bytes())

	// Pre-create the lock file, then make the directory unwritable: the lock can
	// still be opened, the file still read, but the atomic temp-create/rename
	// fails — exactly the persistence-failure path.
	if err := os.WriteFile(path+lockSuffix, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	cred, err := s.Get(context.Background())
	if err != nil {
		t.Fatalf("Get should succeed on the in-memory token despite a write failure: %v", err)
	}
	if !cred.Exp.After(time.Now().Add(time.Hour)) {
		t.Errorf("returned credential is not the freshly refreshed one: exp=%s", cred.Exp)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("expected exactly one refresh, got %d", n)
	}
	// A second Get is served from memory with no further network refresh.
	if _, err := s.Get(context.Background()); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("second Get must be served from memory; server calls = %d", n)
	}
}

// TestRefreshTransientStatusesAreRetryable proves a 429/5xx/timeout refresh is
// classified as transient, not as a terminal "run codex login" authentication
// failure that would tell the client to stop retrying.
func TestRefreshTransientStatusesAreRetryable(t *testing.T) {
	cases := []struct {
		status int
		kind   apierr.Type
	}{
		{http.StatusTooManyRequests, apierr.TypeRateLimit},
		{http.StatusServiceUnavailable, apierr.TypeOverloaded},
		{http.StatusBadGateway, apierr.TypeAPI},
	}
	for _, tc := range cases {
		f := newFakeOAuth(t)
		f.status = tc.status
		s, _ := newTestSource(t, f, authJSON{
			access:    makeJWT(time.Now().Add(5 * time.Second)),
			refresh:   "refresh-old",
			accountID: "acct-transient",
		}.bytes())

		_, err := s.Get(context.Background())
		if err == nil {
			t.Fatalf("status %d: expected an error", tc.status)
		}
		var ae *apierr.Error
		if !errors.As(err, &ae) {
			t.Fatalf("status %d: error is not *apierr.Error: %v", tc.status, err)
		}
		if ae.Kind != tc.kind {
			t.Errorf("status %d: Kind = %s, want %s", tc.status, ae.Kind, tc.kind)
		}
		if ae.Kind == apierr.TypeAuthentication || strings.Contains(err.Error(), "codex login") {
			t.Errorf("status %d wrongly classified as terminal auth: %v", tc.status, err)
		}
	}
}

// ---- write-back: unknown-key preservation + refresh rotation ---------------

func TestRefreshPreservesUnknownKeysAndRotatesRefreshToken(t *testing.T) {
	f := newFakeOAuth(t)
	f.newRefreshTok = "brand-new-refresh"

	unknownTop := json.RawMessage(`{"cli_owned":true,"nested":{"a":1}}`)
	unknownTok := json.RawMessage(`[1,2,3]`)

	body := authJSON{
		openAIAPIKey: "sk-unused",
		lastRefresh:  time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		access:       makeJWT(time.Now().Add(10 * time.Second)), // stale
		idToken:      "old-id",
		refresh:      "old-refresh",
		accountID:    "acct-keep",
		unknownTop:   unknownTop,
		unknownTok:   unknownTok,
	}.bytes()

	// Capture the exact raw bytes of the unknown keys as written.
	origTop, origTokens := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &origTop); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(origTop[keyTokens], &origTokens)
	wantTop := append([]byte(nil), origTop["unknown_top"]...)
	wantTok := append([]byte(nil), origTokens["unknown_tok"]...)

	s, path := newTestSource(t, f, body)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	cred, err := s.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cred.AccountID != "acct-keep" {
		t.Errorf("AccountID = %q", cred.AccountID)
	}

	top, tokens := readTokens(t, path)

	if !bytes.Equal(top["unknown_top"], wantTop) {
		t.Errorf("unknown top-level key not preserved byte-for-byte:\n got %s\nwant %s", top["unknown_top"], wantTop)
	}
	if !bytes.Equal(tokens["unknown_tok"], wantTok) {
		t.Errorf("unknown tokens sub-key not preserved byte-for-byte:\n got %s\nwant %s", tokens["unknown_tok"], wantTok)
	}
	if got := rawString(t, tokens[keyRefreshToken]); got != "brand-new-refresh" {
		t.Errorf("rotated refresh_token = %q, want brand-new-refresh", got)
	}
	if got := rawString(t, top["OPENAI_API_KEY"]); got != "sk-unused" {
		t.Errorf("unrelated known key OPENAI_API_KEY not preserved: %q", got)
	}
	// last_refresh advanced to ~now.
	lr := rawString(t, top[keyLastRefresh])
	tlr, err := time.Parse(time.RFC3339, lr)
	if err != nil {
		t.Fatalf("last_refresh not RFC3339: %q", lr)
	}
	if time.Since(tlr) > time.Minute {
		t.Errorf("last_refresh not advanced: %s", lr)
	}

	// File mode preserved and no temp file left behind.
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != after.Mode().Perm() {
		t.Errorf("mode changed: %v -> %v", before.Mode().Perm(), after.Mode().Perm())
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "auth.json.tmp*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

// ---- in-process singleflight ----------------------------------------------

func TestConcurrentGetTriggersSingleRefresh(t *testing.T) {
	f := newFakeOAuth(t)
	f.gate = make(chan struct{}) // hold every refresh in-flight until released

	s, _ := newTestSource(t, f, authJSON{
		access:    makeJWT(time.Now().Add(5 * time.Second)), // stale
		refresh:   "refresh-old",
		accountID: "acct-sf",
	}.bytes())

	const n = 24
	var ready sync.WaitGroup
	ready.Add(n)
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			ready.Done()
			_, err := s.Get(context.Background())
			results <- err
		}()
	}
	ready.Wait()

	// Wait until the single leader has reached the server, then give the other
	// goroutines time to coalesce onto it before releasing the gate.
	deadline := time.Now().Add(2 * time.Second)
	for f.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(f.gate)

	for i := 0; i < n; i++ {
		if err := <-results; err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
	}
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("expected exactly ONE refresh across %d concurrent Gets, got %d", n, got)
	}
}

// ---- cross-process flock: re-read under lock, skip network -----------------

func TestCrossProcessRefreshSkipsNetworkWhenPeerRefreshed(t *testing.T) {
	f := newFakeOAuth(t)
	s, path := newTestSource(t, f, authJSON{
		access:    makeJWT(time.Now().Add(5 * time.Second)), // stale => wants refresh
		refresh:   "refresh-old",
		accountID: "acct-xp",
	}.bytes())

	// Act as a peer process: grab the same advisory lock the Source will need.
	lockPath := path + lockSuffix
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("peer flock: %v", err)
	}

	// Start a Get; it must block trying to take the lock we hold.
	done := make(chan struct {
		cred Credential
		err  error
	}, 1)
	go func() {
		c, e := s.Get(context.Background())
		done <- struct {
			cred Credential
			err  error
		}{c, e}
	}()

	// Give the Source time to read the stale file and block on the lock.
	time.Sleep(150 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("Get returned while the peer held the lock; it should have blocked")
	default:
	}

	// Peer refreshes the file itself (fresh token), then releases the lock.
	freshAccess := makeJWT(time.Now().Add(3 * time.Hour))
	fresh := authJSON{
		access:    freshAccess,
		refresh:   "refresh-peer",
		accountID: "acct-xp",
	}.bytes()
	if err := os.WriteFile(path, fresh, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("peer unlock: %v", err)
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("Get after peer refresh: %v", res.err)
	}
	if res.cred.AccessToken != freshAccess {
		t.Error("Get did not adopt the peer-refreshed token")
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("Get must skip the network when the peer already refreshed; server calls = %d", n)
	}
}

// ---- Invalidate ------------------------------------------------------------

func TestInvalidateForcesRefresh(t *testing.T) {
	f := newFakeOAuth(t)
	s, _ := newTestSource(t, f, authJSON{
		access:    makeJWT(time.Now().Add(1 * time.Hour)), // fresh
		refresh:   "refresh-old",
		accountID: "acct-inv",
	}.bytes())

	cred, err := s.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if f.calls.Load() != 0 {
		t.Fatalf("fresh token should not refresh, calls=%d", f.calls.Load())
	}

	s.Invalidate(cred)

	newCred, err := s.Get(context.Background())
	if err != nil {
		t.Fatalf("Get after Invalidate: %v", err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("Invalidate must force exactly one refresh, calls=%d", f.calls.Load())
	}
	if newCred.AccessToken == cred.AccessToken {
		t.Error("token unchanged after forced refresh")
	}
}

// ---- error surfaces --------------------------------------------------------

func TestMissingAccountIDIsActionable(t *testing.T) {
	f := newFakeOAuth(t)
	s, _ := newTestSource(t, f, authJSON{
		access:      makeJWT(time.Now().Add(1 * time.Hour)),
		refresh:     "refresh-old",
		omitAccount: true,
	}.bytes())

	_, err := s.Get(context.Background())
	if err == nil {
		t.Fatal("expected an error for missing account_id")
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not an *apierr.Error: %v", err)
	}
	if ae.Kind != apierr.TypeAuthentication {
		t.Errorf("Kind = %s, want authentication_error", ae.Kind)
	}
	msg := err.Error()
	if !strings.Contains(msg, "account_id") || !strings.Contains(msg, "codex login") {
		t.Errorf("message not actionable: %q", msg)
	}
	if f.calls.Load() != 0 {
		t.Error("must not attempt a network refresh when account_id is missing")
	}
}

func TestRefreshFailureIsTerminalAndActionable(t *testing.T) {
	f := newFakeOAuth(t)
	f.status = http.StatusBadRequest // reject the refresh
	s, _ := newTestSource(t, f, authJSON{
		access:    makeJWT(time.Now().Add(5 * time.Second)), // stale => triggers refresh
		refresh:   "refresh-old",
		accountID: "acct-fail",
	}.bytes())

	_, err := s.Get(context.Background())
	if err == nil {
		t.Fatal("expected a terminal error on refresh failure")
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not an *apierr.Error: %v", err)
	}
	if !strings.Contains(err.Error(), "codex login") {
		t.Errorf("refresh failure message not actionable: %q", err.Error())
	}
	// The failed refresh must not have echoed any token material.
	for _, leak := range []string{"refresh-old", "Bearer"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaked %q: %v", leak, err.Error())
		}
	}
}

func TestMissingFileIsActionable(t *testing.T) {
	f := newFakeOAuth(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json") // never created
	s, err := New(Options{Path: path, TokenURL: f.srv.URL, ClientID: "x", HTTPClient: f.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Get(context.Background())
	if err == nil {
		t.Fatal("expected an error for a missing auth.json")
	}
	if !strings.Contains(err.Error(), "codex login") {
		t.Errorf("message not actionable: %q", err.Error())
	}
}

func TestNewRequiresPath(t *testing.T) {
	if _, err := New(Options{}); !errors.Is(err, ErrNoPath) {
		t.Fatalf("New with empty path = %v, want ErrNoPath", err)
	}
}

// ---- redaction / no token in logs ------------------------------------------

func TestRefreshDoesNotLogTokenValues(t *testing.T) {
	f := newFakeOAuth(t)
	f.newRefreshTok = "SECRET-REFRESH-DO-NOT-LOG"

	var logBuf bytes.Buffer
	logger := slogTextLogger(&logBuf)

	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	body := authJSON{
		access:    makeJWT(time.Now().Add(5 * time.Second)),
		refresh:   "SECRET-INPUT-REFRESH",
		accountID: "acct-log",
	}.bytes()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Path:       path,
		TokenURL:   f.srv.URL,
		ClientID:   "x",
		HTTPClient: f.srv.Client(),
		Logger:     logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := s.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	out := logBuf.String()
	for _, leak := range []string{
		"SECRET-REFRESH-DO-NOT-LOG",
		"SECRET-INPUT-REFRESH",
		cred.AccessToken,
	} {
		if leak != "" && strings.Contains(out, leak) {
			t.Errorf("log output leaked a token value: %q in %q", leak, out)
		}
	}
	// The account id must appear only as a hash, never in the clear.
	if strings.Contains(out, "acct-log") {
		t.Errorf("log output leaked the account id in the clear: %q", out)
	}
}
