package providerquota

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
)

// canaryPassword is a distinctive userinfo password; no error text may carry it.
const canaryPassword = "UserinfoPwCanary-7d41"

// TestEndpointURLsRefuseUserinfoQueryAndFragment: a configured endpoint URL is
// where an access token is sent, so a URL carrying credentials, a query or a
// fragment must be refused at construction (and the refusal must not echo it).
func TestEndpointURLsRefuseUserinfoQueryAndFragment(t *testing.T) {
	bad := map[string]string{
		"userinfo":          "https://user:" + canaryPassword + "@example.invalid/usage",
		"userinfo no pass":  "https://" + canaryPassword + "@example.invalid/usage",
		"query":             "https://example.invalid/usage?k=" + canaryPassword,
		"empty query":       "https://example.invalid/usage?",
		"fragment":          "https://example.invalid/usage#" + canaryPassword,
		"non-http scheme":   "ftp://example.invalid/usage",
		"no host":           "https:///usage",
		"userinfo loopback": "http://u:" + canaryPassword + "@127.0.0.1:1/usage",
		"userinfo and port": "https://u:" + canaryPassword + "@example.invalid:8443/usage",
	}
	constructors := map[string]func(rawURL string) error{
		"NewAnthropicClient": func(u string) error {
			_, err := NewAnthropicClient(AnthropicOptions{URL: u})
			return err
		},
		"NewDeepSeekClient": func(u string) error {
			_, err := NewDeepSeekClient(DeepSeekOptions{BaseURL: u, APIKey: "deepseek-key-canary"})
			return err
		},
	}
	for cname, construct := range constructors {
		for name, raw := range bad {
			t.Run(cname+"/"+name, func(t *testing.T) {
				err := construct(raw)
				if err == nil {
					t.Fatalf("%s accepted %q", cname, raw)
				}
				var qerr *Error
				if !errors.As(err, &qerr) || qerr.Code != CodeConfiguration {
					t.Errorf("error = %v, want a %q quota error", err, CodeConfiguration)
				}
				if strings.Contains(err.Error(), canaryPassword) || strings.Contains(err.Error(), "example.invalid") {
					t.Errorf("error echoed the configured URL: %q", err.Error())
				}
			})
		}
	}
}

// TestCodexAppServerGetsIsolatedEnvironment runs the quota read against a fake
// app-server that records its environment, with canary secrets set in the
// parent. None may reach the child, and HOME/CODEX_HOME/TMPDIR/cwd must point
// into the private per-read temp root.
func TestCodexAppServerGetsIsolatedEnvironment(t *testing.T) {
	canaries := map[string]string{
		"DEEPSEEK_API_KEY":          "env-canary-deepseek-3b9f",
		"UTRAQUE_LOCAL_TOKEN":       "env-canary-localtoken-71cc",
		"UTRAQUE_UNRELATED_TOKEN":   "env-canary-unrelated-a02e",
		"ANTHROPIC_API_KEY":         "env-canary-anthropic-55d1",
		"OPENAI_API_KEY":            "env-canary-openai-c8b6",
		"AWS_SECRET_ACCESS_KEY":     "env-canary-aws-19e4",
		"UTRAQUE_CANARY_NOT_SECRET": "env-canary-plain-6f70",
	}
	for k, v := range canaries {
		t.Setenv(k, v)
	}

	envDump := filepath.Join(t.TempDir(), "child-env.txt")
	script := writeFakeAppServer(t, `
OUT='`+envDump+`'
env > "$OUT"
printf 'CWD=%s\n' "$(pwd)" >> "$OUT"
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"id":1,"result":{"userAgent":"fake","platformFamily":"unix","platformOs":"test"}}'
      ;;
    *'"method":"account/login/start"'*)
      printf '%s\n' '{"method":"account/login/completed","params":{"loginId":null,"success":true,"error":null}}'
      printf '%s\n' '{"id":2,"result":{"type":"chatgptAuthTokens"}}'
      ;;
    *'"method":"account/read"'*)
      printf '%s\n' '{"id":3,"result":{"account":{"type":"chatgpt","planType":"plus"},"requiresOpenaiAuth":true}}'
      ;;
    *'"method":"account/rateLimits/read"'*)
      printf '%s\n' '{"id":4,"result":{"rateLimits":{"limitId":"legacy"},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":1789135200}}}}}'
      ;;
  esac
done
`)
	tempRoot := t.TempDir()
	client, err := NewCodexClient(CodexOptions{Command: []string{script}, TempRoot: tempRoot, Timeout: 5 * time.Second, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeCredentialSource{cred: auth.Credential{AccessToken: "codex-access-secret", AccountID: "account-private-id"}}
	if _, err := client.ReadCredential(context.Background(), source, source.cred); err != nil {
		t.Fatalf("ReadCredential: %v", err)
	}

	raw, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatalf("fake app-server never recorded its environment: %v", err)
	}
	dump := string(raw)
	vars := map[string]string{}
	for _, line := range strings.Split(dump, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			vars[k] = v
		}
	}

	for name, value := range canaries {
		if _, present := vars[name]; present {
			t.Errorf("child environment contains %s", name)
		}
		if strings.Contains(dump, value) {
			t.Errorf("child environment leaked the value of %s", name)
		}
	}
	if strings.Contains(dump, "codex-access-secret") || strings.Contains(dump, "account-private-id") {
		t.Error("credential leaked into the child environment")
	}

	// MkdirTemp(tempRoot, "utraque-codex-app-server-") names the private root.
	// The shell's pwd is the physical path, so compare that one resolved.
	privatePrefix := filepath.Join(tempRoot, "utraque-codex-app-server-")
	resolvedRoot, err := filepath.EvalSymlinks(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	resolvedPrefix := filepath.Join(resolvedRoot, "utraque-codex-app-server-")
	for _, name := range []string{"HOME", "CODEX_HOME", "TMPDIR"} {
		if v := vars[name]; !strings.HasPrefix(v, privatePrefix) {
			t.Errorf("%s=%q is not inside the private temp root %q*", name, v, privatePrefix)
		}
	}
	if v := vars["CWD"]; !strings.HasPrefix(v, resolvedPrefix) {
		t.Errorf("working directory %q is not inside the private temp root %q*", v, resolvedPrefix)
	}
	if vars["HOME"] == "" || vars["HOME"] != vars["CODEX_HOME"] {
		t.Errorf("HOME=%q CODEX_HOME=%q should be the same private home", vars["HOME"], vars["CODEX_HOME"])
	}
	if real, ok := os.LookupEnv("HOME"); ok && real != "" && vars["HOME"] == real {
		t.Error("child HOME is the parent's real HOME")
	}
}
