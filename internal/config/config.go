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
	"math"
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
	DefaultDeepSeekBaseURL     = "https://api.deepseek.com/anthropic"
	DefaultCCUsageRunner       = "bunx"
	DefaultCCUsageVersion      = "latest"
	DefaultCodexExecutable     = "codex"
	DefaultProviderCacheTTL    = 30 * time.Second
	DefaultProviderTimeout     = 90 * time.Second

	// DefaultIdleTimeout is 0 — self-exit off — on purpose. Idle self-exit
	// only makes sense once something can bring the daemon back: launchd
	// socket activation, which lands in a later phase. Until then a
	// non-zero default would silently kill a working proxy after a quiet
	// period and leave the next request with a connection refused. Set
	// UTRAQUE_IDLE_TIMEOUT explicitly to opt in.
	DefaultIdleTimeout = time.Duration(0)

	// DefaultLaunchdIdleTimeout is what the idle timeout becomes when launchd
	// handed us the listening socket and nothing set UTRAQUE_IDLE_TIMEOUT.
	// Under socket activation self-exit is free — launchd keeps the socket and
	// re-launches on the next connection — so an hour of quiet is a good
	// default there while remaining off for a manual start.
	DefaultLaunchdIdleTimeout = time.Hour

	// DefaultLaunchdSocketName is the key the plist's Sockets dictionary uses.
	// launchd addresses inherited sockets by that key, so the plist and the
	// binary have to agree on it.
	DefaultLaunchdSocketName = "Listener"

	DefaultLogLevel  = "info"
	DefaultLogFormat = "json"

	// Codex auth-leg defaults. The auth file is resolved from the environment
	// (see resolveCodexAuthFile) rather than being a fixed string, so it has no
	// Default* constant. ClientID is OpenAI's public Codex CLI OAuth client id,
	// not a secret.
	// DefaultCodexBaseURL is the undocumented Codex backend root the Codex CLI
	// itself uses, for both the model catalog and /responses. It is overridable
	// so tests (and only tests) can aim the leg at a fake upstream: the real
	// host is never contacted by the test suite. It restates
	// internal/codex/wire.DefaultBaseURL because config imports no internal
	// package; TestDefaultCodexBaseURLMatchesWire keeps the two in step.
	DefaultCodexBaseURL = "https://chatgpt.com/backend-api/codex"

	DefaultCodexTokenURL    = "https://auth.openai.com/oauth/token"
	DefaultCodexClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultCodexRefreshSkew = 120 * time.Second
	DefaultCodexLockTimeout = 10 * time.Second

	// Transport* are the accepted values of UTRAQUE_CODEX_TRANSPORT: which TLS
	// stack the Codex leg dials chatgpt.com with. They mirror the transport
	// package's Mode* constants, which config deliberately does not import —
	// this enum is validated here the same way log.level and log.format are.
	//
	// TransportModeStd is the standard library, the stack the whole proxy was built
	// and live-verified against. TransportModeUTLS presents a Chrome-shaped TLS
	// ClientHello instead, for the day Cloudflare fingerprint-gates a plain Go
	// client. Only the handshake differs — no forged browser headers — and a
	// hand-rolled TLS stack is a strictly larger attack surface, so it is never
	// the default.
	TransportModeStd  = "std"
	TransportModeUTLS = "utls"
	// TransportModeAuto starts on std and switches to uTLS, once and permanently,
	// the first time the upstream answers with a bot/TLS gate.
	TransportModeAuto = "auto"

	// DefaultCodexTransport is auto: no cost while no gate exists, no outage if
	// one appears. It is deliberately not utls — as of the live end-to-end
	// verification no gate has ever been observed on chatgpt.com.
	DefaultCodexTransport = TransportModeAuto

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
	EnvListen               = EnvPrefix + "LISTEN"
	EnvLocalToken           = EnvPrefix + "LOCAL_TOKEN"
	EnvMaxBodyBytes         = EnvPrefix + "MAX_BODY_BYTES"
	EnvUpstreamIdleTimeout  = EnvPrefix + "UPSTREAM_IDLE_TIMEOUT"
	EnvAnthropicBaseURL     = EnvPrefix + "ANTHROPIC_BASE_URL"
	EnvDeepSeekBaseURL      = EnvPrefix + "DEEPSEEK_BASE_URL"
	EnvDeepSeekAPIKeyFile   = EnvPrefix + "DEEPSEEK_API_KEY_FILE"
	EnvCCUsageRunner        = EnvPrefix + "CCUSAGE_RUNNER"
	EnvCCUsageExecutable    = EnvPrefix + "CCUSAGE_EXECUTABLE"
	EnvCCUsageVersion       = EnvPrefix + "CCUSAGE_VERSION"
	EnvCodexExecutable      = EnvPrefix + "CODEX_EXECUTABLE"
	EnvProviderCacheTTL     = EnvPrefix + "PROVIDER_CACHE_TTL"
	EnvProviderTimeout      = EnvPrefix + "PROVIDER_TIMEOUT"
	EnvClaudePlan           = EnvPrefix + "CLAUDE_PLAN"
	EnvClaudePlanMultiplier = EnvPrefix + "CLAUDE_PLAN_MULTIPLIER"
	EnvIdleTimeout          = EnvPrefix + "IDLE_TIMEOUT"
	EnvLaunchdSocketName    = EnvPrefix + "LAUNCHD_SOCKET"
	EnvLogLevel             = EnvPrefix + "LOG_LEVEL"
	EnvLogFormat            = EnvPrefix + "LOG_FORMAT"

	EnvCodexBaseURL       = EnvPrefix + "CODEX_BASE_URL"
	EnvCodexAuthFile      = EnvPrefix + "CODEX_AUTH_FILE"
	EnvCodexCacheFile     = EnvPrefix + "CODEX_CACHE_FILE"
	EnvCodexTokenURL      = EnvPrefix + "CODEX_TOKEN_URL"
	EnvCodexRefreshSkew   = EnvPrefix + "CODEX_REFRESH_SKEW"
	EnvCodexLockTimeout   = EnvPrefix + "CODEX_LOCK_TIMEOUT"
	EnvCodexTransport     = EnvPrefix + "CODEX_TRANSPORT"
	EnvCodexClientVersion = EnvPrefix + "CODEX_CLIENT_VERSION"

	// EnvRoutingAliasOverrides pins how irregular Codex slugs decompose into
	// aliases. See Routing.AliasOverrides for the format.
	EnvRoutingAliasOverrides = EnvPrefix + "ROUTING_ALIAS_OVERRIDES"

	// EnvCodexHome is the Codex CLI's own variable and is deliberately not
	// UTRAQUE_-prefixed: pointing utraque at the same CODEX_HOME the CLI uses
	// keeps both reading the one auth.json.
	EnvCodexHome = "CODEX_HOME"

	// EnvDeepSeekAPIKey is DeepSeek's conventional SDK variable. It is read
	// unprefixed so one credential can be shared with the vendor's own tools.
	EnvDeepSeekAPIKey = "DEEPSEEK_API_KEY"
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

// DeepSeek configures the dedicated Anthropic-compatible API leg. APIKey is a
// secret and is never rendered by String or LogValue. APIKeyFile records only
// the source path when the explicit file setting was used.
type DeepSeek struct {
	BaseURL    string // UTRAQUE_DEEPSEEK_BASE_URL
	APIKey     string // DEEPSEEK_API_KEY or the contents of APIKeyFile
	APIKeyFile string // UTRAQUE_DEEPSEEK_API_KEY_FILE
}

// Configured reports whether the DeepSeek leg has a usable credential.
func (d DeepSeek) Configured() bool { return d.APIKey != "" }

// Idle configures launchd-friendly self-exit. A Timeout of 0 disables it.
type Idle struct {
	Timeout time.Duration // UTRAQUE_IDLE_TIMEOUT

	// Explicit records that UTRAQUE_IDLE_TIMEOUT was actually set. It is what
	// lets socket activation supply DefaultLaunchdIdleTimeout without
	// overriding an operator who deliberately asked for 0 (never exit).
	Explicit bool
}

// Launchd configures macOS socket activation.
type Launchd struct {
	// SocketName is the plist Sockets key whose descriptors we adopt.
	// UTRAQUE_LAUNCHD_SOCKET. Irrelevant when not started by launchd.
	SocketName string
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
	// CacheFile is utraque's OWN catalog cache file (never the Codex CLI's
	// models_cache.json). Resolved from UTRAQUE_CODEX_CACHE_FILE, else a file
	// under the user cache directory. Empty disables the on-disk cache (the
	// catalog runs memory-only). Empty on a bare Default(); LoadFrom fills it
	// when a user cache directory is available.
	CacheFile string
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
	// Transport selects the TLS stack the leg dials the Codex backend with:
	// TransportModeAuto (default), TransportModeStd, or TransportModeUTLS.
	// UTRAQUE_CODEX_TRANSPORT.
	Transport string
	// ClientVersion is sent as the client_version query parameter on every
	// GET {base}/models request (see catalog.Options.ClientVersion) and is
	// also recorded in utraque's own on-disk catalog cache. Not a secret — it
	// is a Codex CLI version string. An empty value means production startup
	// must discover it from UTRAQUE_CODEX_EXECUTABLE; an explicit
	// UTRAQUE_CODEX_CLIENT_VERSION bypasses discovery.
	ClientVersion string
}

// AliasOverride pins how one Codex slug decomposes into router aliases, for a
// slug the alias grammar parses wrongly or not at all. No slug in the current
// catalog needs one, so none ship; "gpt-5.3-codex-spark", which did until it was
// retired upstream, is the worked example: two trailing tokens, with the codename
// "spark" rather than the last token "codex".
//
// Nothing here is a secret; it is a routing table, and it is logged in full.
type AliasOverride struct {
	// Slug is the upstream model slug the override applies to.
	Slug string
	// Codename is the rolling alias the slug should answer to ("spark").
	Codename string
	// Version is the version used to build the pinned alias ("5.3" gives
	// "spark-5.3") and to rank the slug for the rolling name.
	Version string
	// Modifier is a size/variant token ("mini") for a codename-less slug. It
	// never wins a bare codename alias.
	Modifier string
}

// String renders the override in the same syntax the environment accepts.
func (a AliasOverride) String() string {
	out := a.Slug + "=" + a.Codename + ":" + a.Version
	if a.Modifier != "" {
		out += ":" + a.Modifier
	}
	return out
}

// Routing configures how model names map onto upstream slugs.
type Routing struct {
	// AliasOverrides is the routing.alias_overrides escape hatch, read from
	// UTRAQUE_ROUTING_ALIAS_OVERRIDES as a comma-separated list of
	//
	//	<slug>=<codename>:<version>[:<modifier>]
	//
	// e.g. "gpt-5.3-codex-spark=spark:5.3". An override is consulted before the
	// grammar, so it is the way to make a newly-shipped irregular slug routable
	// without a new build.
	AliasOverrides []AliasOverride
}

// Log configures the slog handler.
type Log struct {
	Level  string // UTRAQUE_LOG_LEVEL:  debug|info|warn|error
	Format string // UTRAQUE_LOG_FORMAT: json|text
}

// Reporting configures the loopback-only provider report. Most external
// helper availability is checked only when the endpoint is requested, so a
// missing ccusage/Claude-plan helper cannot prevent inference from starting.
// CodexExecutable is the exception: when Codex.ClientVersion is not set
// explicitly, startup runs CodexExecutable --version to discover it (see
// Codex.ClientVersion and cmd/utraque's resolveCodexClientVersion), so a
// missing or misconfigured Codex executable does block startup in that case.
type Reporting struct {
	CCUsageRunner        string
	CCUsageExecutable    string
	CCUsageVersion       string
	CodexExecutable      string
	CacheTTL             time.Duration
	Timeout              time.Duration
	ClaudePlan           string
	ClaudePlanMultiplier *float64
}

// Config is the whole configuration surface except request tracing:
// UTRAQUE_TRACE_DIR is read directly by internal/obs (TracerFromEnv), not
// through Config. LocalToken is a secret and is never rendered in full by
// String or LogValue.
type Config struct {
	Listen     string // UTRAQUE_LISTEN
	LocalToken string // UTRAQUE_LOCAL_TOKEN (secret)
	Limits     Limits
	Anthropic  Anthropic
	DeepSeek   DeepSeek
	Codex      Codex
	Routing    Routing
	Idle       Idle
	Launchd    Launchd
	Log        Log
	Reporting  Reporting
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
		DeepSeek:  DeepSeek{BaseURL: DefaultDeepSeekBaseURL},
		Codex: Codex{
			// AuthFile is intentionally empty here: a bare Default() performs no
			// environment or filesystem lookups. LoadFrom resolves it.
			BaseURL:     DefaultCodexBaseURL,
			TokenURL:    DefaultCodexTokenURL,
			ClientID:    DefaultCodexClientID,
			RefreshSkew: DefaultCodexRefreshSkew,
			LockTimeout: DefaultCodexLockTimeout,
			Transport:   DefaultCodexTransport,
		},
		Idle:    Idle{Timeout: DefaultIdleTimeout},
		Launchd: Launchd{SocketName: DefaultLaunchdSocketName},
		Log:     Log{Level: DefaultLogLevel, Format: DefaultLogFormat},
		Reporting: Reporting{
			CCUsageRunner: DefaultCCUsageRunner, CCUsageVersion: DefaultCCUsageVersion,
			CodexExecutable: DefaultCodexExecutable, CacheTTL: DefaultProviderCacheTTL,
			Timeout: DefaultProviderTimeout,
		},
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
	setString(EnvDeepSeekBaseURL, &c.DeepSeek.BaseURL)
	setString(EnvCodexBaseURL, &c.Codex.BaseURL)
	setString(EnvCodexTokenURL, &c.Codex.TokenURL)
	setString(EnvCodexTransport, &c.Codex.Transport)
	setString(EnvCodexClientVersion, &c.Codex.ClientVersion)
	setString(EnvLaunchdSocketName, &c.Launchd.SocketName)
	setString(EnvLogLevel, &c.Log.Level)
	setString(EnvLogFormat, &c.Log.Format)
	setString(EnvCCUsageRunner, &c.Reporting.CCUsageRunner)
	setString(EnvCCUsageExecutable, &c.Reporting.CCUsageExecutable)
	setString(EnvCCUsageVersion, &c.Reporting.CCUsageVersion)
	setString(EnvCodexExecutable, &c.Reporting.CodexExecutable)
	setString(EnvClaudePlan, &c.Reporting.ClaudePlan)

	c.Codex.AuthFile = resolveCodexAuthFile(getenv)
	c.Codex.CacheFile = resolveCodexCacheFile(getenv)
	if v, ok := lookup(getenv, EnvDeepSeekAPIKeyFile); ok {
		c.DeepSeek.APIKeyFile = expandHome(strings.TrimSpace(v), getenv)
		key, err := os.ReadFile(c.DeepSeek.APIKeyFile)
		if err != nil {
			return Config{}, fmt.Errorf("config: %s: %w", EnvDeepSeekAPIKeyFile, err)
		}
		c.DeepSeek.APIKey = strings.TrimSpace(string(key))
		if c.DeepSeek.APIKey == "" {
			return Config{}, fmt.Errorf("config: %s names an empty key file", EnvDeepSeekAPIKeyFile)
		}
	} else if v, ok := lookup(getenv, EnvDeepSeekAPIKey); ok {
		c.DeepSeek.APIKey = strings.TrimSpace(v)
	}

	if v, ok := lookup(getenv, EnvRoutingAliasOverrides); ok {
		overrides, err := parseAliasOverrides(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: %s: %w", EnvRoutingAliasOverrides, err)
		}
		c.Routing.AliasOverrides = overrides
	}

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
	// The idle timeout tracks whether it was set as well as what it is: an
	// explicit 0 means "never self-exit" and must survive socket activation
	// substituting DefaultLaunchdIdleTimeout for an unset value.
	if err := setDuration(EnvIdleTimeout, &c.Idle.Timeout); err != nil {
		return Config{}, err
	}
	if _, ok := lookup(getenv, EnvIdleTimeout); ok {
		c.Idle.Explicit = true
	}
	if err := setDuration(EnvCodexRefreshSkew, &c.Codex.RefreshSkew); err != nil {
		return Config{}, err
	}
	if err := setDuration(EnvCodexLockTimeout, &c.Codex.LockTimeout); err != nil {
		return Config{}, err
	}
	if err := setDuration(EnvProviderCacheTTL, &c.Reporting.CacheTTL); err != nil {
		return Config{}, err
	}
	if err := setDuration(EnvProviderTimeout, &c.Reporting.Timeout); err != nil {
		return Config{}, err
	}
	if v, ok := lookup(getenv, EnvClaudePlanMultiplier); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return Config{}, fmt.Errorf("config: %s must be a finite number", EnvClaudePlanMultiplier)
		}
		c.Reporting.ClaudePlanMultiplier = &f
	}

	c.Codex.BaseURL = strings.TrimRight(strings.TrimSpace(c.Codex.BaseURL), "/")
	c.Codex.TokenURL = strings.TrimRight(strings.TrimSpace(c.Codex.TokenURL), "/")
	c.Codex.Transport = strings.ToLower(strings.TrimSpace(c.Codex.Transport))
	c.Codex.ClientVersion = strings.TrimSpace(c.Codex.ClientVersion)
	c.Listen = strings.TrimSpace(c.Listen)
	c.Launchd.SocketName = strings.TrimSpace(c.Launchd.SocketName)
	c.Log.Level = strings.ToLower(strings.TrimSpace(c.Log.Level))
	c.Log.Format = strings.ToLower(strings.TrimSpace(c.Log.Format))
	c.Anthropic.BaseURL = strings.TrimRight(strings.TrimSpace(c.Anthropic.BaseURL), "/")
	c.DeepSeek.BaseURL = strings.TrimRight(strings.TrimSpace(c.DeepSeek.BaseURL), "/")
	c.Reporting.CCUsageRunner = strings.TrimSpace(c.Reporting.CCUsageRunner)
	c.Reporting.CCUsageExecutable = strings.TrimSpace(c.Reporting.CCUsageExecutable)
	c.Reporting.CCUsageVersion = strings.TrimSpace(c.Reporting.CCUsageVersion)
	c.Reporting.CodexExecutable = strings.TrimSpace(c.Reporting.CodexExecutable)
	c.Reporting.ClaudePlan = strings.TrimSpace(c.Reporting.ClaudePlan)

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

// parseAliasOverrides reads the comma-separated
// "<slug>=<codename>:<version>[:<modifier>]" list. It rejects anything it
// cannot place rather than silently dropping it: a typo here means a model that
// does not route, and a startup failure names the problem while a silent skip
// hides it until someone picks the model.
func parseAliasOverrides(raw string) ([]AliasOverride, error) {
	var out []AliasOverride
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		slug, spec, ok := strings.Cut(entry, "=")
		slug = strings.ToLower(strings.TrimSpace(slug))
		if !ok || slug == "" {
			return nil, fmt.Errorf("override %q: want <slug>=<codename>:<version>[:<modifier>]", entry)
		}
		parts := strings.Split(spec, ":")
		if len(parts) > 3 {
			return nil, fmt.Errorf("override %q: too many \":\"-separated fields", entry)
		}
		ov := AliasOverride{Slug: slug, Codename: strings.ToLower(strings.TrimSpace(parts[0]))}
		if len(parts) > 1 {
			ov.Version = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			ov.Modifier = strings.ToLower(strings.TrimSpace(parts[2]))
		}
		if ov.Codename == "" && ov.Version == "" {
			return nil, fmt.Errorf("override %q: needs at least a codename or a version", entry)
		}
		out = append(out, ov)
	}
	return out, nil
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
	if strings.TrimSpace(c.Reporting.CCUsageExecutable) != c.Reporting.CCUsageExecutable || strings.ContainsRune(c.Reporting.CCUsageExecutable, 0) {
		return fmt.Errorf("config: %s must not have surrounding whitespace or NUL", EnvCCUsageExecutable)
	}
	if c.Reporting.CCUsageExecutable == "" {
		if c.Reporting.CCUsageRunner == "" || strings.TrimSpace(c.Reporting.CCUsageRunner) != c.Reporting.CCUsageRunner || strings.ContainsRune(c.Reporting.CCUsageRunner, 0) {
			return fmt.Errorf("config: %s must name an executable", EnvCCUsageRunner)
		}
		if c.Reporting.CCUsageVersion == "" || strings.ContainsAny(c.Reporting.CCUsageVersion, " \t\r\n\x00") {
			return fmt.Errorf("config: %s must be a single version token", EnvCCUsageVersion)
		}
	}
	if c.Reporting.CodexExecutable == "" || strings.TrimSpace(c.Reporting.CodexExecutable) != c.Reporting.CodexExecutable || strings.ContainsRune(c.Reporting.CodexExecutable, 0) {
		return fmt.Errorf("config: %s must name an executable", EnvCodexExecutable)
	}
	if c.Reporting.CacheTTL <= 0 {
		return fmt.Errorf("config: %s must be positive", EnvProviderCacheTTL)
	}
	if c.Reporting.Timeout <= 0 {
		return fmt.Errorf("config: %s must be positive", EnvProviderTimeout)
	}
	if c.Reporting.ClaudePlanMultiplier != nil && *c.Reporting.ClaudePlanMultiplier <= 0 {
		return fmt.Errorf("config: %s must be positive", EnvClaudePlanMultiplier)
	}
	if len(c.Reporting.ClaudePlan) > 256 || strings.IndexFunc(c.Reporting.ClaudePlan, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return fmt.Errorf("config: %s must be a short printable label", EnvClaudePlan)
	}
	if c.Launchd.SocketName == "" {
		return fmt.Errorf("config: %s must not be empty", EnvLaunchdSocketName)
	}

	if err := validateEndpoint(EnvAnthropicBaseURL, c.Anthropic.BaseURL); err != nil {
		return err
	}
	if err := validateEndpoint(EnvDeepSeekBaseURL, c.DeepSeek.BaseURL); err != nil {
		return err
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
	// ClientVersion may be empty here. It is the sentinel that tells production
	// startup to discover the installed Codex CLI version before constructing
	// the catalog client. Keeping discovery out of config makes Default and
	// LoadFrom deterministic and free of subprocess side effects.
	// Both Codex endpoints must be plain https/http URLs with no embedded
	// credentials — the same rule as the Anthropic base URL, since a
	// misconfigured value is printed to stderr on failure.
	if err := validateEndpoint(EnvCodexBaseURL, c.Codex.BaseURL); err != nil {
		return err
	}
	if err := validateEndpoint(EnvCodexTokenURL, c.Codex.TokenURL); err != nil {
		return err
	}
	// A typo here must not silently fall back to a transport the operator did
	// not ask for: "utsl" quietly meaning "std" would look identical to a
	// working uTLS switch right up until the gate it was set for.
	switch c.Codex.Transport {
	case TransportModeAuto, TransportModeStd, TransportModeUTLS:
	default:
		return fmt.Errorf("config: %s %q: want %s|%s|%s",
			EnvCodexTransport, c.Codex.Transport, TransportModeAuto, TransportModeStd, TransportModeUTLS)
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
	fmt.Fprintf(&b, " deepseek.base_url=%s", RedactURL(c.DeepSeek.BaseURL))
	fmt.Fprintf(&b, " deepseek.configured=%t", c.DeepSeek.Configured())
	fmt.Fprintf(&b, " deepseek.api_key_file=%s", c.DeepSeek.APIKeyFile)
	fmt.Fprintf(&b, " codex.base_url=%s", RedactURL(c.Codex.BaseURL))
	fmt.Fprintf(&b, " codex.auth_file=%s", c.Codex.AuthFile)
	fmt.Fprintf(&b, " codex.cache_file=%s", c.Codex.CacheFile)
	fmt.Fprintf(&b, " codex.token_url=%s", RedactURL(c.Codex.TokenURL))
	fmt.Fprintf(&b, " codex.client_id=%s", c.Codex.ClientID)
	fmt.Fprintf(&b, " codex.refresh_skew=%s", c.Codex.RefreshSkew)
	fmt.Fprintf(&b, " codex.lock_timeout=%s", c.Codex.LockTimeout)
	fmt.Fprintf(&b, " codex.transport=%s", c.Codex.Transport)
	fmt.Fprintf(&b, " codex.client_version=%s", c.Codex.ClientVersion)
	fmt.Fprintf(&b, " routing.alias_overrides=[%s]", joinOverrides(c.Routing.AliasOverrides))
	fmt.Fprintf(&b, " idle_timeout=%s", c.Idle.Timeout)
	fmt.Fprintf(&b, " launchd.socket=%s", c.Launchd.SocketName)
	fmt.Fprintf(&b, " log.level=%s", c.Log.Level)
	fmt.Fprintf(&b, " log.format=%s", c.Log.Format)
	fmt.Fprintf(&b, " reporting.ccusage_version=%s", c.Reporting.CCUsageVersion)
	fmt.Fprintf(&b, " reporting.cache_ttl=%s", c.Reporting.CacheTTL)
	fmt.Fprintf(&b, " reporting.timeout=%s", c.Reporting.Timeout)
	fmt.Fprintf(&b, " reporting.claude_plan=%s", c.Reporting.ClaudePlan)
	fmt.Fprintf(&b, " reporting.claude_plan_multiplier=%s", optionalFloat(c.Reporting.ClaudePlanMultiplier))
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
		slog.String("deepseek.base_url", RedactURL(c.DeepSeek.BaseURL)),
		slog.Bool("deepseek.configured", c.DeepSeek.Configured()),
		slog.String("deepseek.api_key_file", c.DeepSeek.APIKeyFile),
		slog.String("codex.base_url", RedactURL(c.Codex.BaseURL)),
		slog.String("codex.auth_file", c.Codex.AuthFile),
		slog.String("codex.cache_file", c.Codex.CacheFile),
		slog.String("codex.token_url", RedactURL(c.Codex.TokenURL)),
		slog.String("codex.client_id", c.Codex.ClientID),
		slog.Duration("codex.refresh_skew", c.Codex.RefreshSkew),
		slog.Duration("codex.lock_timeout", c.Codex.LockTimeout),
		slog.String("codex.transport", c.Codex.Transport),
		slog.String("codex.client_version", c.Codex.ClientVersion),
		slog.String("routing.alias_overrides", joinOverrides(c.Routing.AliasOverrides)),
		slog.Duration("idle_timeout", c.Idle.Timeout),
		slog.String("launchd.socket", c.Launchd.SocketName),
		slog.String("log.level", c.Log.Level),
		slog.String("log.format", c.Log.Format),
		slog.String("reporting.ccusage_version", c.Reporting.CCUsageVersion),
		slog.Duration("reporting.cache_ttl", c.Reporting.CacheTTL),
		slog.Duration("reporting.timeout", c.Reporting.Timeout),
		slog.String("reporting.claude_plan", c.Reporting.ClaudePlan),
		slog.String("reporting.claude_plan_multiplier", optionalFloat(c.Reporting.ClaudePlanMultiplier)),
	)
}

func optionalFloat(v *float64) string {
	if v == nil {
		return "none"
	}
	return strconv.FormatFloat(*v, 'g', -1, 64)
}

// joinOverrides renders the override table for a log line. It carries no
// secret — it is a slug-to-alias mapping.
func joinOverrides(in []AliasOverride) string {
	if len(in) == 0 {
		return ""
	}
	parts := make([]string, len(in))
	for i, o := range in {
		parts[i] = o.String()
	}
	return strings.Join(parts, " ")
}

func tokenField(tok string) string {
	if tok == "" {
		return "none"
	}
	return Fingerprint(tok)
}

// Fingerprint turns a secret into a stable, non-reversible fingerprint. The empty
// string maps to the empty string.
func Fingerprint(s string) string {
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
