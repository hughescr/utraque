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
		Idle:      Idle{Timeout: DefaultIdleTimeout},
		Log:       Log{Level: DefaultLogLevel, Format: DefaultLogFormat},
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
	setString(EnvLogLevel, &c.Log.Level)
	setString(EnvLogFormat, &c.Log.Format)

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
