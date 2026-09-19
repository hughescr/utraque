package config_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/config"
)

const secret = "s3cr3t-local-token-value"

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c, err := config.LoadFrom(envFrom(nil))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.Listen != config.DefaultListen {
		t.Errorf("Listen = %q, want %q", c.Listen, config.DefaultListen)
	}
	if c.Limits.MaxBodyBytes != config.DefaultMaxBodyBytes {
		t.Errorf("MaxBodyBytes = %d, want %d", c.Limits.MaxBodyBytes, int64(config.DefaultMaxBodyBytes))
	}
	if c.Limits.UpstreamIdleTimeout != config.DefaultUpstreamIdleTimeout {
		t.Errorf("UpstreamIdleTimeout = %s", c.Limits.UpstreamIdleTimeout)
	}
	if c.Anthropic.BaseURL != config.DefaultAnthropicBaseURL {
		t.Errorf("BaseURL = %q", c.Anthropic.BaseURL)
	}
	if c.DeepSeek.BaseURL != config.DefaultDeepSeekBaseURL || c.DeepSeek.Configured() {
		t.Errorf("DeepSeek default = %+v", c.DeepSeek)
	}
	if c.Idle.Timeout != config.DefaultIdleTimeout {
		t.Errorf("Idle.Timeout = %s", c.Idle.Timeout)
	}
	if c.Log.Level != config.DefaultLogLevel || c.Log.Format != config.DefaultLogFormat {
		t.Errorf("Log = %+v", c.Log)
	}
	if c.HasLocalToken() {
		t.Error("HasLocalToken = true with no token configured")
	}
	if config.Default().Listen != c.Listen {
		t.Error("Default() disagrees with LoadFrom(empty)")
	}
}

func TestDeepSeekKeyFileWinsAndFailuresAreFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deepseek.key")
	if err := os.WriteFile(path, []byte("  file-key-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.LoadFrom(envFrom(map[string]string{
		"HOME":                       "/home/tester",
		config.EnvDeepSeekAPIKey:     "environment-key-value",
		config.EnvDeepSeekAPIKeyFile: path,
		config.EnvDeepSeekBaseURL:    "http://deepseek.test/anthropic/",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.DeepSeek.APIKey != "file-key-value" || c.DeepSeek.APIKeyFile != path {
		t.Errorf("DeepSeek credential = %+v, want explicit file contents", c.DeepSeek)
	}
	if c.DeepSeek.BaseURL != "http://deepseek.test/anthropic" {
		t.Errorf("DeepSeek.BaseURL = %q", c.DeepSeek.BaseURL)
	}

	for name, file := range map[string]string{
		"missing": filepath.Join(t.TempDir(), "missing"),
		"empty":   filepath.Join(t.TempDir(), "empty"),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "empty" {
				if err := os.WriteFile(file, []byte(" \n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := config.LoadFrom(envFrom(map[string]string{
				"HOME":                       "/home/tester",
				config.EnvDeepSeekAPIKey:     "fallback-must-not-be-used",
				config.EnvDeepSeekAPIKeyFile: file,
			}))
			if err == nil {
				t.Fatal("explicit unreadable/empty key file silently fell back")
			}
		})
	}
}

func TestDeepSeekKeyNeverAppearsInConfigRendering(t *testing.T) {
	c := config.Default()
	c.DeepSeek.APIKey = "sk-deepseek-rendering-secret"
	c.DeepSeek.APIKeyFile = "/private/path/deepseek.key"
	if got := c.String(); strings.Contains(got, c.DeepSeek.APIKey) || !strings.Contains(got, "deepseek.configured=true") {
		t.Fatalf("String() mishandled DeepSeek credential: %s", got)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	log.Info("config", "cfg", c)
	if strings.Contains(buf.String(), c.DeepSeek.APIKey) {
		t.Fatalf("LogValue leaked DeepSeek key: %s", buf.String())
	}
}

func TestReportingExecutablePathsAreNotRendered(t *testing.T) {
	c := config.Default()
	c.Reporting.CCUsageExecutable = "/private/sensitive/bin/ccusage"
	c.Reporting.CCUsageRunner = "/private/sensitive/bin/bunx"
	c.Reporting.CodexExecutable = "/private/sensitive/bin/codex"
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("config", "cfg", c)
	for _, rendered := range []string{c.String(), buf.String()} {
		if strings.Contains(rendered, "/private/sensitive") {
			t.Fatalf("reporting executable path leaked: %s", rendered)
		}
	}
}

func TestEnvOverrides(t *testing.T) {
	multiplier := 4.5
	c, err := config.LoadFrom(envFrom(map[string]string{
		config.EnvListen:               "127.0.0.1:9999",
		config.EnvLocalToken:           secret,
		config.EnvMaxBodyBytes:         "1024",
		config.EnvUpstreamIdleTimeout:  "45s",
		config.EnvAnthropicBaseURL:     "https://example.test/api/",
		config.EnvIdleTimeout:          "15m",
		config.EnvLogLevel:             "DEBUG",
		config.EnvLogFormat:            " Text ",
		config.EnvCCUsageRunner:        "custom-bunx",
		config.EnvCCUsageExecutable:    "/opt/homebrew/bin/ccusage",
		config.EnvCCUsageVersion:       "20.0.20",
		config.EnvCodexExecutable:      "custom-codex",
		config.EnvProviderCacheTTL:     "45s",
		config.EnvProviderTimeout:      "80s",
		config.EnvClaudePlan:           "Max",
		config.EnvClaudePlanMultiplier: strconv.FormatFloat(multiplier, 'f', -1, 64),
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q", c.Listen)
	}
	if c.LocalToken != secret || !c.HasLocalToken() {
		t.Error("LocalToken not applied")
	}
	if c.Limits.MaxBodyBytes != 1024 {
		t.Errorf("MaxBodyBytes = %d", c.Limits.MaxBodyBytes)
	}
	if c.Limits.UpstreamIdleTimeout != 45*time.Second {
		t.Errorf("UpstreamIdleTimeout = %s", c.Limits.UpstreamIdleTimeout)
	}
	if c.Anthropic.BaseURL != "https://example.test/api" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", c.Anthropic.BaseURL)
	}
	if c.Idle.Timeout != 15*time.Minute {
		t.Errorf("Idle.Timeout = %s", c.Idle.Timeout)
	}
	if c.Log.Level != "debug" || c.Log.Format != "text" {
		t.Errorf("Log = %+v, want lowercased and trimmed", c.Log)
	}
	if c.SlogLevel() != slog.LevelDebug {
		t.Errorf("SlogLevel = %v", c.SlogLevel())
	}
	if c.Reporting.CCUsageRunner != "custom-bunx" || c.Reporting.CCUsageExecutable != "/opt/homebrew/bin/ccusage" || c.Reporting.CCUsageVersion != "20.0.20" || c.Reporting.CodexExecutable != "custom-codex" {
		t.Fatalf("reporting executables = %+v", c.Reporting)
	}
	if c.Reporting.CacheTTL != 45*time.Second || c.Reporting.Timeout != 80*time.Second || c.Reporting.ClaudePlan != "Max" || c.Reporting.ClaudePlanMultiplier == nil || *c.Reporting.ClaudePlanMultiplier != multiplier {
		t.Fatalf("reporting config = %+v", c.Reporting)
	}
}

func TestLoadFromParseErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"bad max body":        {config.EnvMaxBodyBytes: "lots"},
		"bad upstream":        {config.EnvUpstreamIdleTimeout: "forever"},
		"bad idle":            {config.EnvIdleTimeout: "1 hour"},
		"bad listen":          {config.EnvListen: "not-a-hostport"},
		"bad base url":        {config.EnvAnthropicBaseURL: "https://u:p@api.anthropic.com"},
		"bad provider ttl":    {config.EnvProviderCacheTTL: "forever"},
		"bad plan multiplier": {config.EnvClaudePlanMultiplier: "NaN"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := config.LoadFrom(envFrom(env)); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

// deploy/install.sh renders UTRAQUE_LISTEN from --node and --port, and brackets
// an IPv6 literal because that is what net.SplitHostPort needs. The bare form
// is what the plist's SockNodeName takes, so both forms have to be settled
// here: bracketed is valid, bare-with-a-port is not. A daemon that rejected its
// own configured address after launchd had already handed it a good socket
// would exit on every activation, once a second, forever.
func TestValidateAcceptsABracketedIPv6Listen(t *testing.T) {
	for _, addr := range []string{"[::1]:8317", "[::]:8317", "localhost:8317"} {
		c := config.Default()
		c.Listen = addr
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() with Listen %q = %v, want nil", addr, err)
		}
	}
	c := config.Default()
	c.Listen = "::1:8317" // the unbracketed form, which is ambiguous
	if err := c.Validate(); err == nil {
		t.Error("Validate() accepted an unbracketed IPv6 listen address")
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*config.Config){
		"empty listen":             func(c *config.Config) { c.Listen = "" },
		"listen no port":           func(c *config.Config) { c.Listen = "127.0.0.1" },
		"listen bad port":          func(c *config.Config) { c.Listen = "127.0.0.1:http" },
		"zero body":                func(c *config.Config) { c.Limits.MaxBodyBytes = 0 },
		"negative body":            func(c *config.Config) { c.Limits.MaxBodyBytes = -1 },
		"negative upstream":        func(c *config.Config) { c.Limits.UpstreamIdleTimeout = -time.Second },
		"negative idle":            func(c *config.Config) { c.Idle.Timeout = -time.Second },
		"empty base url":           func(c *config.Config) { c.Anthropic.BaseURL = "" },
		"relative base url":        func(c *config.Config) { c.Anthropic.BaseURL = "api.anthropic.com" },
		"bad scheme":               func(c *config.Config) { c.Anthropic.BaseURL = "ftp://api.anthropic.com" },
		"userinfo":                 func(c *config.Config) { c.Anthropic.BaseURL = "https://user:pass@api.anthropic.com" },
		"query":                    func(c *config.Config) { c.Anthropic.BaseURL = "https://api.anthropic.com?key=sk-abc" },
		"fragment":                 func(c *config.Config) { c.Anthropic.BaseURL = "https://api.anthropic.com#tok" },
		"bad level":                func(c *config.Config) { c.Log.Level = "verbose" },
		"bad format":               func(c *config.Config) { c.Log.Format = "logfmt" },
		"zero report ttl":          func(c *config.Config) { c.Reporting.CacheTTL = 0 },
		"negative plan multiplier": func(c *config.Config) { n := -1.0; c.Reporting.ClaudePlanMultiplier = &n },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := config.Default()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want an error for %s", name)
			}
		})
	}
}

func TestValidateAcceptsDefault(t *testing.T) {
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("Default() must validate: %v", err)
	}
}

func TestNativeCCUsageExecutableTakesPrecedenceOverRunnerSettings(t *testing.T) {
	c := config.Default()
	c.Reporting.CCUsageExecutable = "/opt/homebrew/bin/ccusage"
	c.Reporting.CCUsageRunner = ""
	c.Reporting.CCUsageVersion = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate with native ccusage: %v", err)
	}
}

func TestSecretNeverRendered(t *testing.T) {
	c := config.Default()
	c.LocalToken = secret

	s := c.String()
	if strings.Contains(s, secret) {
		t.Fatalf("String() leaked the token: %s", s)
	}
	if !strings.Contains(s, "local_token=sha256:") {
		t.Errorf("String() = %s, want a sha256 fingerprint", s)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", "config", c)
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("LogValue leaked the token: %s", buf.String())
	}
	if !strings.Contains(buf.String(), config.Fingerprint(secret)) {
		t.Errorf("LogValue = %s, want fingerprint %s", buf.String(), config.Fingerprint(secret))
	}
}

func TestSecretNeverRenderedForURLCredentials(t *testing.T) {
	c := config.Default()
	// Validate rejects this, but String must stay safe even for a bad value.
	c.Anthropic.BaseURL = "https://user:hunter2@api.anthropic.com/v1?key=sk-leak"
	s := c.String()
	for _, leak := range []string{"hunter2", "sk-leak"} {
		if strings.Contains(s, leak) {
			t.Fatalf("String() leaked %q: %s", leak, s)
		}
	}
}

func TestStringWithoutToken(t *testing.T) {
	if s := config.Default().String(); !strings.Contains(s, "local_token=none") {
		t.Errorf("String() = %s, want local_token=none", s)
	}
}

func TestFingerprint(t *testing.T) {
	if config.Fingerprint("") != "" {
		t.Error(`Fingerprint("") must be ""`)
	}
	got := config.Fingerprint(secret)
	if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+8 {
		t.Fatalf("Fingerprint = %q, want sha256: plus 8 hex", got)
	}
	if got != config.Fingerprint(secret) {
		t.Error("Fingerprint is not stable")
	}
	if got == config.Fingerprint(secret+"x") {
		t.Error("Fingerprint collides on distinct inputs")
	}
	if strings.Contains(got, secret) {
		t.Error("Fingerprint leaked its input")
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"":                                     "",
		"https://api.anthropic.com":            "https://api.anthropic.com",
		"https://u:p@api.anthropic.com/v1":     "https://api.anthropic.com/v1",
		"https://api.anthropic.com/v1?key=sk1": "https://api.anthropic.com/v1",
		"https://api.anthropic.com/v1#sk1":     "https://api.anthropic.com/v1",
	}
	for in, want := range cases {
		if got := config.RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlogLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"":      slog.LevelInfo,
	} {
		c := config.Default()
		c.Log.Level = in
		if got := c.SlogLevel(); got != want {
			t.Errorf("SlogLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLoadFromNilGetenv(t *testing.T) {
	if _, err := config.LoadFrom(nil); err != nil {
		t.Fatalf("LoadFrom(nil) = %v, want defaults", err)
	}
}

func TestLoadReadsProcessEnv(t *testing.T) {
	t.Setenv(config.EnvListen, "127.0.0.1:7777")
	t.Setenv(config.EnvLogLevel, "warn")
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != "127.0.0.1:7777" || c.Log.Level != "warn" {
		t.Errorf("Load did not read the process environment: %+v", c)
	}
}

func TestCodexDefaults(t *testing.T) {
	c, err := config.LoadFrom(envFrom(map[string]string{"HOME": "/home/tester"}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.Codex.TokenURL != config.DefaultCodexTokenURL {
		t.Errorf("TokenURL = %q, want %q", c.Codex.TokenURL, config.DefaultCodexTokenURL)
	}
	if c.Codex.ClientID != config.DefaultCodexClientID {
		t.Errorf("ClientID = %q", c.Codex.ClientID)
	}
	if c.Codex.RefreshSkew != config.DefaultCodexRefreshSkew {
		t.Errorf("RefreshSkew = %s", c.Codex.RefreshSkew)
	}
	if c.Codex.LockTimeout != config.DefaultCodexLockTimeout {
		t.Errorf("LockTimeout = %s", c.Codex.LockTimeout)
	}
	want := filepath.Join("/home/tester", config.CodexDirName, config.CodexAuthFileName)
	if c.Codex.AuthFile != want {
		t.Errorf("AuthFile = %q, want %q", c.Codex.AuthFile, want)
	}
}

func TestCodexAuthFileResolution(t *testing.T) {
	const home = "/home/tester"
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "default under HOME/.codex",
			env:  map[string]string{"HOME": home},
			want: filepath.Join(home, ".codex", "auth.json"),
		},
		{
			name: "CODEX_HOME overrides the directory",
			env:  map[string]string{"HOME": home, config.EnvCodexHome: "/opt/codex"},
			want: "/opt/codex/auth.json",
		},
		{
			name: "UTRAQUE_CODEX_AUTH_FILE overrides the whole path",
			env: map[string]string{
				"HOME":                  home,
				config.EnvCodexHome:     "/opt/codex",
				config.EnvCodexAuthFile: "/custom/place/creds.json",
			},
			want: "/custom/place/creds.json",
		},
		{
			name: "tilde in the explicit override expands to HOME",
			env:  map[string]string{"HOME": home, config.EnvCodexAuthFile: "~/somewhere/auth.json"},
			want: filepath.Join(home, "somewhere", "auth.json"),
		},
		{
			name: "tilde in CODEX_HOME expands to HOME",
			env:  map[string]string{"HOME": home, config.EnvCodexHome: "~/xdg/codex"},
			want: filepath.Join(home, "xdg", "codex", "auth.json"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := config.LoadFrom(envFrom(tc.env))
			if err != nil {
				t.Fatalf("LoadFrom: %v", err)
			}
			if c.Codex.AuthFile != tc.want {
				t.Errorf("AuthFile = %q, want %q", c.Codex.AuthFile, tc.want)
			}
		})
	}
}

func TestCodexEnvOverrides(t *testing.T) {
	c, err := config.LoadFrom(envFrom(map[string]string{
		"HOME":                       "/home/tester",
		config.EnvCodexTokenURL:      "https://auth.example.test/oauth/token/",
		config.EnvCodexRefreshSkew:   "30s",
		config.EnvCodexLockTimeout:   "3s",
		config.EnvCodexClientVersion: "0.999.0",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.Codex.TokenURL != "https://auth.example.test/oauth/token" {
		t.Errorf("TokenURL = %q, want the trailing slash trimmed", c.Codex.TokenURL)
	}
	if c.Codex.RefreshSkew != 30*time.Second {
		t.Errorf("RefreshSkew = %s", c.Codex.RefreshSkew)
	}
	if c.Codex.LockTimeout != 3*time.Second {
		t.Errorf("LockTimeout = %s", c.Codex.LockTimeout)
	}
	if c.Codex.ClientVersion != "0.999.0" {
		t.Errorf("ClientVersion = %q, want the env override", c.Codex.ClientVersion)
	}
}

// TestCodexClientVersionDefaultsToDiscoverySentinel proves config loading is
// pure: startup, rather than Default or LoadFrom, discovers the installed
// Codex CLI version. Once resolved, the value is safe to render in full.
func TestCodexClientVersionDefaultsToDiscoverySentinel(t *testing.T) {
	c := config.Default()
	if c.Codex.ClientVersion != "" {
		t.Errorf("Codex.ClientVersion = %q, want empty discovery sentinel", c.Codex.ClientVersion)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Default().Validate() = %v, want nil", err)
	}

	c.Codex.ClientVersion = "0.999.0"
	s := c.String()
	if !strings.Contains(s, "codex.client_version=0.999.0") {
		t.Errorf("String() = %s, want it to show the resolved client version", s)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", "config", c)
	if !strings.Contains(buf.String(), "0.999.0") {
		t.Errorf("LogValue = %s, want it to show the resolved client version", buf.String())
	}
}

func TestCodexValidateRejects(t *testing.T) {
	cases := map[string]func(*config.Config){
		"negative refresh skew": func(c *config.Config) { c.Codex.RefreshSkew = -time.Second },
		"zero lock timeout":     func(c *config.Config) { c.Codex.LockTimeout = 0 },
		"negative lock timeout": func(c *config.Config) { c.Codex.LockTimeout = -time.Second },
		"empty client id":       func(c *config.Config) { c.Codex.ClientID = "" },
		"empty token url":       func(c *config.Config) { c.Codex.TokenURL = "" },
		"token url bad scheme":  func(c *config.Config) { c.Codex.TokenURL = "ftp://auth.openai.com/x" },
		"token url userinfo":    func(c *config.Config) { c.Codex.TokenURL = "https://u:p@auth.openai.com/x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := config.Default()
			// Default() leaves AuthFile empty; give the rest a valid baseline.
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want an error for %s", name)
			}
		})
	}
}

func TestCodexTokenURLErrorDoesNotLeakUserinfo(t *testing.T) {
	c := config.Default()
	c.Codex.TokenURL = "ftp://user:hunter2@auth.openai.com/oauth/token"
	err := c.Validate()
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("Validate leaked the credential: %v", err)
	}
}

func TestCodexConfigRenderedAndNotSecret(t *testing.T) {
	c, err := config.LoadFrom(envFrom(map[string]string{"HOME": "/home/tester"}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	s := c.String()
	if !strings.Contains(s, "codex.auth_file=") || !strings.Contains(s, "codex.client_id=") {
		t.Errorf("String() missing codex fields: %s", s)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", "config", c)
	if !strings.Contains(buf.String(), "codex.token_url") {
		t.Errorf("LogValue missing codex.token_url: %s", buf.String())
	}
}

// TestValidateDoesNotLeakUserinfoInErrors: config errors are printed to
// stderr, and the userinfo check runs after the scheme and host checks. Those
// earlier messages must not echo a credential-bearing URL back verbatim.
func TestValidateDoesNotLeakUserinfoInErrors(t *testing.T) {
	for _, raw := range []string{
		"ftp://user:hunter2@example.com",
		"gopher://user:hunter2@example.com/x",
		"https://user:hunter2@example.com?token=hunter2",
	} {
		c := config.Default()
		c.Anthropic.BaseURL = raw
		err := c.Validate()
		if err == nil {
			t.Fatalf("Validate(%q) = nil, want an error", raw)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("Validate(%q) leaked the credential: %v", raw, err)
		}
	}
}

// TestAliasOverridesParse covers routing.alias_overrides, the escape hatch for a
// slug the alias grammar cannot place. A malformed entry fails at startup rather
// than being dropped: a silent skip hides the problem until someone picks the
// model and gets a 404.
func TestAliasOverridesParse(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		c, err := config.LoadFrom(envFrom(map[string]string{
			config.EnvRoutingAliasOverrides: "gpt-5.3-codex-spark=spark:5.3, GPT-5.4-Turbo-Mini=:5.4:mini",
		}))
		if err != nil {
			t.Fatalf("LoadFrom: %v", err)
		}
		want := []config.AliasOverride{
			{Slug: "gpt-5.3-codex-spark", Codename: "spark", Version: "5.3"},
			{Slug: "gpt-5.4-turbo-mini", Version: "5.4", Modifier: "mini"},
		}
		if !reflect.DeepEqual(c.Routing.AliasOverrides, want) {
			t.Errorf("overrides = %+v, want %+v", c.Routing.AliasOverrides, want)
		}
		if s := c.String(); !strings.Contains(s, "gpt-5.3-codex-spark=spark:5.3") {
			t.Errorf("String() = %s, want it to name the override", s)
		}
	})

	for _, bad := range []string{"nosep", "=spark:5.3", "gpt-5.9-x=", "gpt-5.9-x=a:b:c:d"} {
		t.Run("rejected "+bad, func(t *testing.T) {
			if _, err := config.LoadFrom(envFrom(map[string]string{
				config.EnvRoutingAliasOverrides: bad,
			})); err == nil {
				t.Errorf("LoadFrom accepted the malformed override %q", bad)
			}
		})
	}
}
