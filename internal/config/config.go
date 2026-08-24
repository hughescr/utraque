// Package config defines utraque's configuration surface: defaults, the
// UTRAQUE_-prefixed environment overrides, validation that rejects
// secret-shaped values, and redaction so a Config can be logged safely.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults. Every one is overridable by the matching Env* variable.
const (
	DefaultListen              = "127.0.0.1:8317"
	DefaultMaxBodyBytes        = 64 << 20 // 64 MiB
	DefaultUpstreamIdleTimeout = 120 * time.Second
	DefaultAnthropicBaseURL    = "https://api.anthropic.com"

	// DefaultIdleTimeout is 0 — self-exit off — on purpose. Idle self-exit
	// only makes sense once something can bring the daemon back: launchd
	// socket activation, which lands in a later phase. Until then a
	// non-zero default would silently kill a working proxy after a quiet
	// period and leave the next request with a connection refused. Set
	// UTRAQUE_IDLE_TIMEOUT explicitly to opt in.
	DefaultIdleTimeout = time.Duration(0)
	DefaultLogLevel    = "info"
	DefaultLogFormat   = "json"

	// Codex auth-leg defaults. The auth file is resolved from the environment
	// (see resolveCodexAuthFile) rather than being a fixed string, so it has no
	// Default* constant. ClientID is OpenAI's public Codex CLI OAuth client id,
	// not a secret.
	// DefaultCodexBaseURL is the undocumented Codex backend root the Codex CLI
	// itself uses, for both the model catalog and /responses. It is overridable
	// so tests (and only tests) can aim the leg at a fake upstream: the real
	// host is never contacted by the test suite.
	DefaultCodexBaseURL = "https://chatgpt.com/backend-api/codex"

	DefaultCodexTokenURL    = "https://auth.openai.com/oauth/token"
	DefaultCodexClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultCodexRefreshSkew = 120 * time.Second
	DefaultCodexLockTimeout = 10 * time.Second

	// CodexDirName / CodexAuthFileName build the default {~/.codex}/auth.json
	// path. CODEX_HOME (the Codex CLI's own variable, unprefixed) overrides the
	// directory; UTRAQUE_CODEX_AUTH_FILE overrides the whole path.
	CodexDirName      = ".codex"
	CodexAuthFileName = "auth.json"

	// CodexCacheDirName / CodexCacheFileName build utraque's OWN catalog cache
	// path under the user cache directory. It is deliberately NOT the Codex
	// CLI's models_cache.json: utraque keeps its own file and never overwrites
	// the CLI's. UTRAQUE_CODEX_CACHE_FILE overrides the whole path.
	CodexCacheDirName  = "utraque"
	CodexCacheFileName = "models_cache.json"

	EnvPrefix = "UTRAQUE_"
)

// Environment variable names.
const (
	EnvListen              = EnvPrefix + "LISTEN"
	EnvLocalToken          = EnvPrefix + "LOCAL_TOKEN"
	EnvMaxBodyBytes        = EnvPrefix + "MAX_BODY_BYTES"
	EnvUpstreamIdleTimeout = EnvPrefix + "UPSTREAM_IDLE_TIMEOUT"
	EnvAnthropicBaseURL    = EnvPrefix + "ANTHROPIC_BASE_URL"
	EnvIdleTimeout         = EnvPrefix + "IDLE_TIMEOUT"
	EnvLogLevel            = EnvPrefix + "LOG_LEVEL"
	EnvLogFormat           = EnvPrefix + "LOG_FORMAT"

	EnvCodexBaseURL     = EnvPrefix + "CODEX_BASE_URL"
	EnvCodexAuthFile    = EnvPrefix + "CODEX_AUTH_FILE"
	EnvCodexCacheFile   = EnvPrefix + "CODEX_CACHE_FILE"
	EnvCodexTokenURL    = EnvPrefix + "CODEX_TOKEN_URL"
	EnvCodexRefreshSkew = EnvPrefix + "CODEX_REFRESH_SKEW"
	EnvCodexLockTimeout = EnvPrefix + "CODEX_LOCK_TIMEOUT"

	// EnvCodexHome is the Codex CLI's own variable and is deliberately not
	// UTRAQUE_-prefixed: pointing utraque at the same CODEX_HOME the CLI uses
	// keeps both reading the one auth.json.
	EnvCodexHome = "CODEX_HOME"
)

// Limits bounds what a single request may cost us.
type Limits struct {
	MaxBodyBytes        int64         // UTRAQUE_MAX_BODY_BYTES
	UpstreamIdleTimeout time.Duration // UTRAQUE_UPSTREAM_IDLE_TIMEOUT
}

// Anthropic configures the pass-through leg.
type Anthropic struct {
	BaseURL string // UTRAQUE_ANTHROPIC_BASE_URL
}

// Idle configures launchd-friendly self-exit. A Timeout of 0 disables it.
type Idle struct {
	Timeout time.Duration // UTRAQUE_IDLE_TIMEOUT
}

// Codex configures the Codex (OpenAI) auth leg: where the credential file
// lives, where refresh tokens are exchanged, and the timing knobs that govern
// pre-emptive refresh and cross-process locking. None of these fields is a
// secret — the tokens themselves live only in AuthFile on disk and are never
// held in Config.
type Codex struct {
	// BaseURL is the Codex backend root used for both the model catalog and the
	// /responses inference endpoint.
	BaseURL string
	// AuthFile is the absolute path to the Codex CLI credential file. It is
	// resolved from UTRAQUE_CODEX_AUTH_FILE, else CODEX_HOME/auth.json, else
	// ~/.codex/auth.json. Empty on a bare Default(); LoadFrom always fills it.
	AuthFile string
	// CachePath is utraque's OWN catalog cache file (never the Codex CLI's
	// models_cache.json). Resolved from UTRAQUE_CODEX_CACHE_FILE, else a file
	// under the user cache directory. Empty disables the on-disk cache (the
	// catalog runs memory-only). Empty on a bare Default(); LoadFrom fills it
	// when a user cache directory is available.
	CachePath string
	// TokenURL is the OAuth token endpoint used to exchange a refresh token.
	TokenURL string
	// ClientID is the public OAuth client id presented on refresh.
	ClientID string
	// RefreshSkew triggers a pre-emptive refresh once the access token is
	// within this long of expiry.
	RefreshSkew time.Duration
	// LockTimeout bounds how long a refresh waits for the cross-process
	// advisory file lock before giving up.
	LockTimeout time.Duration
}

// Log configures the slog handler.
type Log struct {
	Level  string // UTRAQUE_LOG_LEVEL:  debug|info|warn|error
	Format string // UTRAQUE_LOG_FORMAT: json|text
}

// Config is the whole configuration surface. LocalToken is a secret and is
// never rendered in full by String or LogValue.
type Config struct {
	Listen     string // UTRAQUE_LISTEN
	LocalToken string // UTRAQUE_LOCAL_TOKEN (secret)
	Limits     Limits
	Anthropic  Anthropic
	Codex      Codex
	Idle       Idle
	Log        Log
}

var (
	_ slog.LogValuer = Config{}
	_ fmt.Stringer   = Config{}
)

// Default returns the configuration used when no environment overrides are set.
func Default() Config {
	return Config{
		Listen: DefaultListen,
		Limits: Limits{
			MaxBodyBytes:        DefaultMaxBodyBytes,
			UpstreamIdleTimeout: DefaultUpstreamIdleTimeout,
		},
		Anthropic: Anthropic{BaseURL: DefaultAnthropicBaseURL},
		Codex: Codex{
			// AuthFile is intentionally empty here: a bare Default() performs no
			// environment or filesystem lookups. LoadFrom resolves it.
			BaseURL:     DefaultCodexBaseURL,
			TokenURL:    DefaultCodexTokenURL,
			ClientID:    DefaultCodexClientID,
			RefreshSkew: DefaultCodexRefreshSkew,
			LockTimeout: DefaultCodexLockTimeout,
		},
		Idle: Idle{Timeout: DefaultIdleTimeout},
		Log:  Log{Level: DefaultLogLevel, Format: DefaultLogFormat},
	}
}

// Load reads the process environment.
func Load() (Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom applies UTRAQUE_-prefixed overrides from getenv on top of Default
// and validates the result. An empty value counts as "not set", so a default
// cannot be overridden to the empty string.
func LoadFrom(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	c := Default()

	setString := func(key string, dst *string) {
		if v, ok := lookup(getenv, key); ok {
			*dst = v
		}
	}
	setString(EnvListen, &c.Listen)
	setString(EnvLocalToken, &c.LocalToken)
	setString(EnvAnthropicBaseURL, &c.Anthropic.BaseURL)
	setString(EnvCodexBaseURL, &c.Codex.BaseURL)
	setString(EnvCodexTokenURL, &c.Codex.TokenURL)
	setString(EnvLogLevel, &c.Log.Level)
	setString(EnvLogFormat, &c.Log.Format)

	c.Codex.AuthFile = resolveCodexAuthFile(getenv)
	c.Codex.CachePath = resolveCodexCacheFile(getenv)

	if v, ok := lookup(getenv, EnvMaxBodyBytes); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("config: %s: %w", EnvMaxBodyBytes, err)
		}
		c.Limits.MaxBodyBytes = n
	}

	setDuration := func(key string, dst *time.Duration) error {
		v, ok := lookup(getenv, key)
		if !ok {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("config: %s: %w", key, err)
		}
		*dst = d
		return nil
	}
	if err := setDuration(EnvUpstreamIdleTimeout, &c.Limits.UpstreamIdleTimeout); err != nil {
		return Config{}, err
	}
	if err := setDuration(EnvIdleTimeout, &c.Idle.Timeout); err != nil {
		return Config{}, err
	}
	if err := setDuration(EnvCodexRefreshSkew, &c.Codex.RefreshSkew); err != nil {
		return Config{}, err
	}
	if err := setDuration(EnvCodexLockTimeout, &c.Codex.LockTimeout); err != nil {
		return Config{}, err
	}

	c.Codex.BaseURL = strings.TrimRight(strings.TrimSpace(c.Codex.BaseURL), "/")
	c.Codex.TokenURL = strings.TrimRight(strings.TrimSpace(c.Codex.TokenURL), "/")
	c.Listen = strings.TrimSpace(c.Listen)
	c.Log.Level = strings.ToLower(strings.TrimSpace(c.Log.Level))
	c.Log.Format = strings.ToLower(strings.TrimSpace(c.Log.Format))
	c.Anthropic.BaseURL = strings.TrimRight(strings.TrimSpace(c.Anthropic.BaseURL), "/")

	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func lookup(getenv func(string) string, key string) (string, bool) {
	v := getenv(key)
	if v == "" {
		return "", false
	}
	return v, true
}

// resolveCodexAuthFile picks the Codex credential path, most specific first:
// UTRAQUE_CODEX_AUTH_FILE, else CODEX_HOME/auth.json, else ~/.codex/auth.json.
// A leading "~/" is expanded against the resolved home directory. The result
// is a path only; the file is neither opened nor required to exist here.
func resolveCodexAuthFile(getenv func(string) string) string {
	if v, ok := lookup(getenv, EnvCodexAuthFile); ok {
		return expandHome(strings.TrimSpace(v), getenv)
	}
	if v, ok := lookup(getenv, EnvCodexHome); ok {
		return filepath.Join(expandHome(strings.TrimSpace(v), getenv), CodexAuthFileName)
	}
	return filepath.Join(homeDir(getenv), CodexDirName, CodexAuthFileName)
}

// resolveCodexCacheFile picks utraque's own catalog cache path:
// UTRAQUE_CODEX_CACHE_FILE if set (with "~/" expansion), else a file under the
// OS user cache directory. When no cache directory can be determined the result
// is empty, which the catalog treats as "memory only". This is never the Codex
// CLI's models_cache.json.
func resolveCodexCacheFile(getenv func(string) string) string {
	if v, ok := lookup(getenv, EnvCodexCacheFile); ok {
		return expandHome(strings.TrimSpace(v), getenv)
	}
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, CodexCacheDirName, CodexCacheFileName)
}

// homeDir resolves the user's home directory, preferring an explicit HOME so
// the result tracks the getenv passed to LoadFrom (and so tests can steer it),
// and falling back to os.UserHomeDir.
func homeDir(getenv func(string) string) string {
	if h := strings.TrimSpace(getenv("HOME")); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

// expandHome rewrites a leading "~" or "~/" to the home directory. A bare path
// is returned unchanged.
func expandHome(p string, getenv func(string) string) string {
	switch {
	case p == "~":
		return homeDir(getenv)
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(homeDir(getenv), p[2:])
	default:
		return p
	}
}

// Validate reports the first configuration problem, if any. It rejects a
// secret-shaped Anthropic base URL (userinfo, query or fragment) so a
// credential can never ride in a configured URL.
func (c Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("config: %s must not be empty", EnvListen)
	}
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("config: %s %q: %w", EnvListen, c.Listen, err)
	}
	if port == "" {
		return fmt.Errorf("config: %s %q: port must not be empty", EnvListen, c.Listen)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("config: %s %q: port must be a number in 0..65535", EnvListen, c.Listen)
	}

	if c.Limits.MaxBodyBytes <= 0 {
		return fmt.Errorf("config: %s must be positive, got %d", EnvMaxBodyBytes, c.Limits.MaxBodyBytes)
	}
	if c.Limits.UpstreamIdleTimeout < 0 {
		return fmt.Errorf("config: %s must not be negative, got %s", EnvUpstreamIdleTimeout, c.Limits.UpstreamIdleTimeout)
	}
	if c.Idle.Timeout < 0 {
		return fmt.Errorf("config: %s must not be negative, got %s", EnvIdleTimeout, c.Idle.Timeout)
	}

	if c.Anthropic.BaseURL == "" {
		return fmt.Errorf("config: %s must not be empty", EnvAnthropicBaseURL)
	}
	// Every message below renders the URL through RedactURL, and the parse
	// failure renders only url.Error's inner cause. A misconfigured value can
	// carry userinfo credentials, and these errors are printed to stderr:
	// the userinfo check further down runs too late to protect them.
	u, err := url.Parse(c.Anthropic.BaseURL)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("config: %s is not a valid URL: %w", EnvAnthropicBaseURL, ue.Err)
		}
		return fmt.Errorf("config: %s is not a valid URL", EnvAnthropicBaseURL)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("config: %s %q: scheme must be http or https", EnvAnthropicBaseURL, RedactURL(c.Anthropic.BaseURL))
	}
	if u.Host == "" {
		return fmt.Errorf("config: %s %q: missing host", EnvAnthropicBaseURL, RedactURL(c.Anthropic.BaseURL))
	}
	if u.User != nil {
		return fmt.Errorf("config: %s must not contain userinfo credentials", EnvAnthropicBaseURL)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("config: %s must not contain a query string", EnvAnthropicBaseURL)
	}
	if u.Fragment != "" {
		return fmt.Errorf("config: %s must not contain a fragment", EnvAnthropicBaseURL)
	}

	if c.Codex.RefreshSkew < 0 {
		return fmt.Errorf("config: %s must not be negative, got %s", EnvCodexRefreshSkew, c.Codex.RefreshSkew)
	}
	if c.Codex.LockTimeout <= 0 {
		return fmt.Errorf("config: %s must be positive, got %s", EnvCodexLockTimeout, c.Codex.LockTimeout)
	}
	if c.Codex.ClientID == "" {
		return fmt.Errorf("config: codex client id must not be empty")
	}
	// Both Codex endpoints must be plain https/http URLs with no embedded
	// credentials — the same rule as the Anthropic base URL, since a
	// misconfigured value is printed to stderr on failure.
	if err := validateEndpoint(EnvCodexBaseURL, c.Codex.BaseURL); err != nil {
		return err
	}
	if err := validateEndpoint(EnvCodexTokenURL, c.Codex.TokenURL); err != nil {
		return err
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: %s %q: want debug|info|warn|error", EnvLogLevel, c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		return fmt.Errorf("config: %s %q: want json|text", EnvLogFormat, c.Log.Format)
	}
	return nil
}

// validateEndpoint checks one configured URL: present, parseable, http(s), with
// a host and without userinfo, query or fragment. Every message renders the URL
// through RedactURL (or only url.Error's inner cause), because a misconfigured
// value can carry credentials and these errors are printed to stderr.
func validateEndpoint(name, raw string) error {
	if raw == "" {
		return fmt.Errorf("config: %s must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("config: %s is not a valid URL: %w", name, ue.Err)
		}
		return fmt.Errorf("config: %s is not a valid URL", name)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("config: %s %q: scheme must be http or https", name, RedactURL(raw))
	}
	if u.Host == "" {
		return fmt.Errorf("config: %s %q: missing host", name, RedactURL(raw))
	}
	if u.User != nil {
		return fmt.Errorf("config: %s must not contain userinfo credentials", name)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("config: %s must not contain a query string", name)
	}
	if u.Fragment != "" {
		return fmt.Errorf("config: %s must not contain a fragment", name)
	}
	return nil
}

// HasLocalToken reports whether the loopback shared secret is configured.
func (c Config) HasLocalToken() bool { return c.LocalToken != "" }

// SlogLevel maps Log.Level onto a slog.Level, defaulting to info.
func (c Config) SlogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(c.Log.Level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// String renders the configuration on one line with every secret redacted.
func (c Config) String() string {
	var b strings.Builder
	b.WriteString("config{")
	fmt.Fprintf(&b, "listen=%s", c.Listen)
	fmt.Fprintf(&b, " local_token=%s", tokenField(c.LocalToken))
	fmt.Fprintf(&b, " max_body_bytes=%d", c.Limits.MaxBodyBytes)
	fmt.Fprintf(&b, " upstream_idle_timeout=%s", c.Limits.UpstreamIdleTimeout)
	fmt.Fprintf(&b, " anthropic.base_url=%s", RedactURL(c.Anthropic.BaseURL))
	fmt.Fprintf(&b, " codex.base_url=%s", RedactURL(c.Codex.BaseURL))
	fmt.Fprintf(&b, " codex.auth_file=%s", c.Codex.AuthFile)
	fmt.Fprintf(&b, " codex.cache_file=%s", c.Codex.CachePath)
	fmt.Fprintf(&b, " codex.token_url=%s", RedactURL(c.Codex.TokenURL))
	fmt.Fprintf(&b, " codex.client_id=%s", c.Codex.ClientID)
	fmt.Fprintf(&b, " codex.refresh_skew=%s", c.Codex.RefreshSkew)
	fmt.Fprintf(&b, " codex.lock_timeout=%s", c.Codex.LockTimeout)
	fmt.Fprintf(&b, " idle_timeout=%s", c.Idle.Timeout)
	fmt.Fprintf(&b, " log.level=%s", c.Log.Level)
	fmt.Fprintf(&b, " log.format=%s", c.Log.Format)
	b.WriteString("}")
	return b.String()
}

// LogValue implements slog.LogValuer with the same redaction as String.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("listen", c.Listen),
		slog.String("local_token", tokenField(c.LocalToken)),
		slog.Int64("max_body_bytes", c.Limits.MaxBodyBytes),
		slog.Duration("upstream_idle_timeout", c.Limits.UpstreamIdleTimeout),
		slog.String("anthropic.base_url", RedactURL(c.Anthropic.BaseURL)),
		slog.String("codex.base_url", RedactURL(c.Codex.BaseURL)),
		slog.String("codex.auth_file", c.Codex.AuthFile),
		slog.String("codex.cache_file", c.Codex.CachePath),
		slog.String("codex.token_url", RedactURL(c.Codex.TokenURL)),
		slog.String("codex.client_id", c.Codex.ClientID),
		slog.Duration("codex.refresh_skew", c.Codex.RefreshSkew),
		slog.Duration("codex.lock_timeout", c.Codex.LockTimeout),
		slog.Duration("idle_timeout", c.Idle.Timeout),
		slog.String("log.level", c.Log.Level),
		slog.String("log.format", c.Log.Format),
	)
}

func tokenField(tok string) string {
	if tok == "" {
		return "none"
	}
	return Redact(tok)
}

// Redact turns a secret into a stable, non-reversible fingerprint. The empty
// string maps to the empty string.
func Redact(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:4])
}

// RedactURL strips userinfo, query and fragment from a URL so it is safe to
// log. An unparseable input renders as "invalid-url".
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid-url"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}
