// Package auth manages the Codex (OpenAI) OAuth credential utraque replays
// against the ChatGPT backend. It reads {CODEX_HOME:-~/.codex}/auth.json,
// decodes the access token's expiry, refreshes the token when it is near
// expiry (or was rejected upstream), and writes the rotated credential back —
// all while sharing the file safely with the Codex CLI, which owns it.
//
// The credential file is precious: clobbering it can log the user out of their
// own Codex CLI. Every write is therefore made atomic (temp file + rename) and
// guarded by a cross-process advisory lock, under which the file is re-read so
// a refresh another process already performed is adopted rather than repeated.
// In-process, concurrent refreshes collapse to one via singleflight. Unknown
// keys the Codex CLI owns are preserved verbatim across a write-back.
//
// This package never logs, returns, or otherwise discloses a token value.
package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/obs"
)

// Default timing knobs, used when Options leaves them zero.
const (
	defaultRefreshSkew = 120 * time.Second
	defaultLockTimeout = 10 * time.Second
	defaultHTTPTimeout = 30 * time.Second
	lockSuffix         = ".lock"
)

// Credential is a usable Codex access token plus the account it authorises and
// the moment it expires. It carries no refresh token: refreshing is the
// Source's job, never the caller's.
type Credential struct {
	AccessToken string
	AccountID   string
	Exp         time.Time
}

// Credential state names reported by Peek. They describe what a subsequent Get
// would do, without disclosing any token material.
const (
	// StateOK: a usable credential comfortably before expiry; Get serves it
	// from cache with no network.
	StateOK = "ok"
	// StateStale: a credential that exists but is at/near expiry or was marked
	// invalid; the next Get would refresh it over the network.
	StateStale = "stale"
	// StateMissing: no usable credential — the file is absent, unreadable, or
	// lacks the required account_id/access_token fields; the user must run
	// `codex login`.
	StateMissing = "missing"
)

// Status is the non-secret snapshot of the Codex credential that /healthz
// reports. It is produced by Peek without any network call and carries no token
// value — only the coarse state and, when known, the time until expiry.
type Status struct {
	// State is one of StateOK, StateStale, or StateMissing.
	State string
	// ExpiresIn is the time until the access token expires. It is only
	// meaningful when HasExpiry is true; it may be negative for an already
	// expired token.
	ExpiresIn time.Duration
	// HasExpiry reports whether ExpiresIn reflects a decoded token expiry. It is
	// false when there is no usable credential, or the token's expiry could not
	// be decoded.
	HasExpiry bool
}

// CredentialSource yields a currently-valid Codex credential and lets a caller
// report one as rejected.
//
// Get returns a credential that is valid now, refreshing first if it is near
// or past expiry. Invalidate marks a credential stale so the next Get refreshes
// even if the token has not yet expired; a leg calls it after the upstream
// answers 401.
type CredentialSource interface {
	Get(ctx context.Context) (Credential, error)
	Invalidate(cred Credential)
}

// Options configures a Source. Path is required; the rest default.
type Options struct {
	// Path is the absolute path to the Codex auth.json.
	Path string
	// TokenURL is the OAuth token endpoint. Defaults to the OpenAI endpoint.
	TokenURL string
	// ClientID is the OAuth client id presented on refresh.
	ClientID string
	// RefreshSkew triggers a pre-emptive refresh this long before expiry.
	RefreshSkew time.Duration
	// LockTimeout bounds the wait for the cross-process advisory lock.
	LockTimeout time.Duration
	// HTTPClient performs the refresh POST. Defaults to a client with a modest
	// timeout. Tests inject a client aimed at a fake endpoint.
	HTTPClient *http.Client
	// Now supplies the current time. Defaults to time.Now. Tests override it.
	Now func() time.Time
	// Logger receives redacted operational logs. Defaults to slog.Default.
	Logger *slog.Logger
}

// Source is a file-backed CredentialSource. It is safe for concurrent use.
type Source struct {
	path        string
	lockPath    string
	tokenURL    string
	clientID    string
	refreshSkew time.Duration
	lockTimeout time.Duration
	httpClient  *http.Client
	now         func() time.Time
	log         *slog.Logger

	// group collapses concurrent refresh/load work for this file into one
	// execution, keyed on the file path.
	group singleflight.Group

	mu          sync.Mutex
	cached      *cachedCred
	invalidated map[string]struct{} // access tokens forced stale by Invalidate
	// invalGen counts Invalidate calls. A loader captures it before it decides a
	// token is usable and passes it to store, which refuses to cache (or clear
	// the stale-marker of) a token that a concurrent Invalidate marked in the
	// meantime — closing the load-vs-Invalidate race.
	invalGen uint64
}

type cachedCred struct {
	cred Credential
	stat statInfo
}

// statInfo is the cheap external-change fingerprint: a changed size or mtime
// means the file was rewritten (e.g. by the Codex CLI) and must be re-read.
type statInfo struct {
	size  int64
	mtime time.Time
}

var (
	_ CredentialSource = (*Source)(nil)

	// ErrNoPath is returned by New when Options.Path is empty.
	ErrNoPath = errors.New("auth: Options.Path is required")
)

// New builds a Source. Only Path is mandatory.
func New(opts Options) (*Source, error) {
	if opts.Path == "" {
		return nil, ErrNoPath
	}
	s := &Source{
		path:        opts.Path,
		lockPath:    opts.Path + lockSuffix,
		tokenURL:    opts.TokenURL,
		clientID:    opts.ClientID,
		refreshSkew: opts.RefreshSkew,
		lockTimeout: opts.LockTimeout,
		httpClient:  opts.HTTPClient,
		now:         opts.Now,
		log:         opts.Logger,
		invalidated: map[string]struct{}{},
	}
	if s.refreshSkew <= 0 {
		s.refreshSkew = defaultRefreshSkew
	}
	if s.lockTimeout <= 0 {
		s.lockTimeout = defaultLockTimeout
	}
	if s.httpClient == nil {
		s.httpClient = &http.Client{
			Timeout: defaultHTTPTimeout,
			// Never follow a redirect on the refresh POST: it carries the refresh
			// token, and following a 3xx would replay it to whatever host the
			// response named. Treat the 3xx as the final response instead.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// getAttempts bounds how many times Get will re-run the loader to escape a
// shared load that began before the caller's Invalidate. Two is enough: the
// second attempt starts after the first has completed, so it can only join a
// load that itself started after the Invalidate landed.
const getAttempts = 2

// Get returns a valid credential, refreshing if necessary.
func (s *Source) Get(ctx context.Context) (Credential, error) {
	var cred Credential
	for attempt := 0; attempt < getAttempts; attempt++ {
		if c, ok := s.fastPath(); ok {
			return c, nil
		}
		// singleflight ensures N concurrent Gets for this file drive at most one
		// disk read + refresh; the rest share its result.
		v, err, _ := s.group.Do(s.path, func() (any, error) {
			return s.load(ctx)
		})
		if err != nil {
			return Credential{}, err
		}
		cred = v.(Credential)
		// A shared load may have STARTED before this caller's Invalidate landed,
		// in which case its result is the very token we just reported rejected.
		// Serving it would hand the caller a known-dead credential and cost them
		// a 401 that one more load would have fixed, so drive the loader again.
		if !s.isInvalidated(cred.AccessToken) {
			return cred, nil
		}
	}
	// Every attempt came back invalidated (e.g. a refresh that cannot rotate the
	// token). Return it anyway rather than failing: the caller's own retry
	// policy — which can see the whole request — decides what to do with a 401.
	return cred, nil
}

// maxInvalidated bounds how many rejected access tokens are remembered. The set
// is only ever a hint ("do not serve this one from cache"), and every entry is a
// plaintext token held for the life of the process, so it must not grow without
// limit. Past the bound the whole set is dropped and rebuilt from the newest
// entry: the worst case is one extra upstream 401, which immediately re-marks
// the token.
const maxInvalidated = 32

// Invalidate marks cred stale. The next Get refreshes even if cred has not yet
// expired. A blank token is ignored.
func (s *Source) Invalidate(cred Credential) {
	if cred.AccessToken == "" {
		return
	}
	s.mu.Lock()
	s.invalGen++
	if _, known := s.invalidated[cred.AccessToken]; !known && len(s.invalidated) >= maxInvalidated {
		s.invalidated = make(map[string]struct{}, 1)
	}
	s.invalidated[cred.AccessToken] = struct{}{}
	if s.cached != nil && s.cached.cred.AccessToken == cred.AccessToken {
		s.cached = nil
	}
	s.mu.Unlock()
}

// Peek reports the current credential state without refreshing. It stats and
// reads the auth file and decodes the token expiry locally, but never contacts
// the network and never writes the file, so a caller (e.g. /healthz) can poll
// it cheaply. A missing or invalid file reports StateMissing; a token that is
// at/near expiry or has been Invalidate-marked reports StateStale; a usable,
// comfortably-fresh token reports StateOK.
func (s *Source) Peek() Status {
	af, _, _, err := s.readFile()
	if err != nil {
		return Status{State: StateMissing}
	}
	cred, err := s.credFrom(af)
	if err != nil {
		return Status{State: StateMissing}
	}
	st := Status{
		ExpiresIn: cred.Exp.Sub(s.now()),
		HasExpiry: !cred.Exp.IsZero(),
	}
	switch {
	case s.isInvalidated(cred.AccessToken):
		st.State = StateStale
	case s.fresh(cred):
		st.State = StateOK
	default:
		st.State = StateStale
	}
	return st
}

// fastPath returns the cached credential when the file is unchanged on disk,
// the token is not invalidated, and it is comfortably before expiry. Any doubt
// falls through to the singleflight-guarded slow path.
func (s *Source) fastPath() (Credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached == nil {
		return Credential{}, false
	}
	st, err := os.Stat(s.path)
	if err != nil {
		return Credential{}, false
	}
	if (statInfo{st.Size(), st.ModTime()}) != s.cached.stat {
		return Credential{}, false
	}
	if _, bad := s.invalidated[s.cached.cred.AccessToken]; bad {
		return Credential{}, false
	}
	if !s.freshLocked(s.cached.cred) {
		return Credential{}, false
	}
	return s.cached.cred, true
}

// load reads the file, and refreshes under the cross-process lock if the token
// is stale or invalidated. It runs inside singleflight, so only one instance
// executes per file at a time.
func (s *Source) load(ctx context.Context) (Credential, error) {
	gen := s.generation() // capture before we judge freshness (see store)
	af, mode, stat, err := s.readFile()
	if err != nil {
		return Credential{}, err
	}
	cred, err := s.credFrom(af)
	if err != nil {
		return Credential{}, err
	}
	if s.fresh(cred) && !s.isInvalidated(cred.AccessToken) {
		s.store(cred, stat, gen)
		return cred, nil
	}
	return s.refreshUnderLock(ctx, mode)
}

// refreshUnderLock takes the advisory lock, re-reads the file (another process
// may have refreshed while we waited), refreshes over the network only if the
// token is still stale, writes the result back atomically, and caches it.
func (s *Source) refreshUnderLock(ctx context.Context, mode os.FileMode) (Credential, error) {
	gen := s.generation()
	release, err := acquireFileLock(ctx, s.lockPath, s.lockTimeout)
	if err != nil {
		switch {
		case errors.Is(err, errLockTimeout):
			return Credential{}, apierr.WithStatus(http.StatusServiceUnavailable, apierr.TypeOverloaded,
				"codex token refresh is busy (another process holds the lock); please retry")
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return Credential{}, apierr.WithStatus(http.StatusServiceUnavailable, apierr.TypeOverloaded,
				"codex token refresh was interrupted while waiting for the auth lock; please retry")
		default:
			return Credential{}, apierr.Wrap(err, apierr.TypeAPI, "codex: acquire auth lock")
		}
	}
	defer release()

	// Re-read under the lock: the holder we waited on may have refreshed.
	af, freshMode, stat, err := s.readFile()
	if err != nil {
		return Credential{}, err
	}
	if freshMode != 0 {
		mode = freshMode
	}
	cred, err := s.credFrom(af)
	if err != nil {
		return Credential{}, err
	}
	if s.fresh(cred) && !s.isInvalidated(cred.AccessToken) {
		// Someone else refreshed it; adopt without touching the network.
		s.store(cred, stat, gen)
		s.log.Debug("codex token already fresh under lock; skipping network refresh",
			obs.HashAttr("account", cred.AccountID))
		return cred, nil
	}

	refreshTok := af.tokenString(keyRefreshToken)
	if refreshTok == "" {
		return Credential{}, apierr.Authentication(
			"codex auth.json has no tokens.refresh_token; run `codex login`")
	}

	res, err := s.requestRefresh(ctx, refreshTok)
	if err != nil {
		return Credential{}, err
	}

	if err := af.setTokenString(keyAccessToken, res.AccessToken); err != nil {
		return Credential{}, apierr.Wrap(err, apierr.TypeAPI, "codex: encode access token")
	}
	if res.IDToken != "" {
		if err := af.setTokenString(keyIDToken, res.IDToken); err != nil {
			return Credential{}, apierr.Wrap(err, apierr.TypeAPI, "codex: encode id token")
		}
	}
	// OpenAI rotates refresh tokens; persist a new one, keep the old otherwise.
	if res.RefreshToken != "" {
		if err := af.setTokenString(keyRefreshToken, res.RefreshToken); err != nil {
			return Credential{}, apierr.Wrap(err, apierr.TypeAPI, "codex: encode refresh token")
		}
	}
	if err := af.setTopString(keyLastRefresh, s.now().UTC().Format(time.RFC3339)); err != nil {
		return Credential{}, apierr.Wrap(err, apierr.TypeAPI, "codex: encode last_refresh")
	}

	data, err := af.marshal()
	if err != nil {
		return Credential{}, apierr.Wrap(err, apierr.TypeAPI, "codex: encode auth.json")
	}

	newCred, err := s.credFrom(af)
	if err != nil {
		return Credential{}, err
	}

	// Defence-in-depth against a writer that does NOT honour our advisory lock
	// (e.g. a Codex CLI build that just truncates auth.json): if the file changed
	// on disk since our under-lock re-read, another process wrote it during our
	// network refresh. Never clobber that newer file. Adopt it if it now yields a
	// usable credential; otherwise fail transiently so the caller retries rather
	// than overwriting someone else's rotated token.
	if s.fileChangedSince(stat) {
		s.log.Warn("codex auth.json changed on disk during refresh; not overwriting the external write",
			obs.HashAttr("account", newCred.AccountID))
		if adopted, ok := s.adoptFromDisk(gen); ok {
			return adopted, nil
		}
		return Credential{}, apierr.WithStatus(http.StatusServiceUnavailable, apierr.TypeOverloaded,
			"codex auth.json was updated by another process during refresh; please retry")
	}

	if err := writeAuthFileAtomic(s.path, data, mode); err != nil {
		// The refresh itself SUCCEEDED — the old refresh token was already
		// redeemed and (if rotated) is now dead upstream. Discarding the result
		// here would strand us with a dead token on disk and force a re-login, so
		// keep the freshly rotated credential in memory for this run and surface
		// the persistence failure as a warning rather than losing the token.
		s.storeMemoryOnly(newCred, gen)
		s.log.Warn("codex token refreshed but auth.json could not be persisted; using it from memory this run",
			obs.HashAttr("account", newCred.AccountID), slog.String("err", err.Error()))
		return newCred, nil
	}

	st, statErr := os.Stat(s.path)
	if statErr == nil {
		s.store(newCred, statInfo{st.Size(), st.ModTime()}, gen)
	} else {
		s.store(newCred, statInfo{}, gen)
	}
	s.log.Info("refreshed codex token",
		obs.HashAttr("account", newCred.AccountID),
		slog.Time("expires_at", newCred.Exp))
	return newCred, nil
}

// fileChangedSince reports whether the auth file's cheap fingerprint differs
// from base (or can no longer be stat'd). A stat error is treated as "changed"
// so an ambiguous state never licenses an overwrite.
func (s *Source) fileChangedSince(base statInfo) bool {
	st, err := os.Stat(s.path)
	if err != nil {
		return true
	}
	return statInfo{st.Size(), st.ModTime()} != base
}

// adoptFromDisk re-reads the file and, if it now holds a usable, fresh, not-
// invalidated credential, caches and returns it. It is how we take over a token
// an external writer just wrote instead of clobbering it.
func (s *Source) adoptFromDisk(gen uint64) (Credential, bool) {
	af, _, stat, err := s.readFile()
	if err != nil {
		return Credential{}, false
	}
	cred, err := s.credFrom(af)
	if err != nil {
		return Credential{}, false
	}
	if !s.fresh(cred) || s.isInvalidated(cred.AccessToken) {
		return Credential{}, false
	}
	s.store(cred, stat, gen)
	return cred, true
}

// readFile stats, reads, and parses the auth file. A missing or unreadable file
// becomes an actionable authentication error. The returned mode is the file's
// permission bits (0 when unknown).
func (s *Source) readFile() (*authFile, os.FileMode, statInfo, error) {
	st, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, statInfo{}, apierr.Authentication(
				"codex auth.json not found at %s; run `codex login`", s.path)
		}
		return nil, 0, statInfo{}, apierr.Wrap(err, apierr.TypeAPI, "codex: stat auth.json")
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return nil, 0, statInfo{}, apierr.Wrap(err, apierr.TypeAPI, "codex: read auth.json")
	}
	af, err := parseAuthFile(b)
	if err != nil {
		return nil, 0, statInfo{}, apierr.Authentication(
			"codex auth.json is not valid JSON; run `codex login`")
	}
	return af, st.Mode().Perm(), statInfo{st.Size(), st.ModTime()}, nil
}

// credFrom derives a Credential from a parsed file, requiring the fields the
// Codex backend needs. A missing account_id is the actionable error the plan
// calls out.
func (s *Source) credFrom(af *authFile) (Credential, error) {
	account := af.tokenString(keyAccountID)
	if account == "" {
		return Credential{}, apierr.Authentication(
			"codex auth.json has no tokens.account_id; run `codex login`")
	}
	access := af.tokenString(keyAccessToken)
	if access == "" {
		return Credential{}, apierr.Authentication(
			"codex auth.json has no tokens.access_token; run `codex login`")
	}
	// A zero expiry (undecodable) leaves the credential looking already
	// expired, which forces a refresh — the safe direction.
	exp, _ := accessTokenExpiry(access, af.topString(keyLastRefresh), fallbackTTL)
	return Credential{AccessToken: access, AccountID: account, Exp: exp}, nil
}

// fresh reports whether cred is comfortably before expiry.
func (s *Source) fresh(cred Credential) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.freshLocked(cred)
}

func (s *Source) freshLocked(cred Credential) bool {
	return cred.Exp.Sub(s.now()) >= s.refreshSkew
}

func (s *Source) isInvalidated(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.invalidated[tok]
	return ok
}

// generation returns the current Invalidate counter. A loader captures it before
// judging a token usable and passes it to store, so store can detect an
// Invalidate that raced between the freshness check and the cache write.
func (s *Source) generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalGen
}

// store caches cred and clears any stale-marker for its (now current) token.
// If an Invalidate landed since gen was captured AND it marked this very token,
// we neither cache nor clear it: the caller's "fresh" verdict is stale, so the
// next Get must refresh rather than serve a token already reported rejected.
func (s *Source) store(cred Credential, stat statInfo, gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.invalGen != gen {
		if _, bad := s.invalidated[cred.AccessToken]; bad {
			return
		}
	}
	s.cached = &cachedCred{cred: cred, stat: stat}
	delete(s.invalidated, cred.AccessToken)
}

// storeMemoryOnly caches cred without a matching on-disk write. It fingerprints
// the file as it currently stands so the fast path keeps serving cred until the
// file changes or the token nears expiry. Used only when a refresh succeeded but
// persistence failed.
func (s *Source) storeMemoryOnly(cred Credential, gen uint64) {
	if st, err := os.Stat(s.path); err == nil {
		s.store(cred, statInfo{st.Size(), st.ModTime()}, gen)
	} else {
		s.store(cred, statInfo{}, gen)
	}
}
