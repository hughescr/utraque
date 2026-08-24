package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/config"
)

func TestLaunchdSocketDefault(t *testing.T) {
	c, err := config.LoadFrom(func(string) string { return "" })
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.Launchd.SocketName != config.DefaultLaunchdSocketName {
		t.Errorf("SocketName = %q, want %q", c.Launchd.SocketName, config.DefaultLaunchdSocketName)
	}
	// The plist and the binary have to name the same socket, so the default is
	// worth pinning: changing it silently would strand every installed plist.
	if config.DefaultLaunchdSocketName != "Listener" {
		t.Errorf("DefaultLaunchdSocketName = %q; the shipped plist says \"Listener\"",
			config.DefaultLaunchdSocketName)
	}
}

func TestLaunchdSocketOverride(t *testing.T) {
	c, err := config.LoadFrom(env(map[string]string{
		config.EnvLaunchdSocketName: "  Proxy  ",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.Launchd.SocketName != "Proxy" {
		t.Errorf("SocketName = %q, want the trimmed override", c.Launchd.SocketName)
	}
}

func TestEmptyLaunchdSocketIsRejected(t *testing.T) {
	c := config.Default()
	c.Launchd.SocketName = ""
	if err := c.Validate(); err == nil {
		t.Error("an empty launchd socket name must not validate")
	}
}

// TestIdleExplicitTracksTheEnvironment: whether the idle timeout was SET is a
// separate fact from what it is, because socket activation supplies a default
// only for an unset one — and an explicit 0 must keep meaning "never exit".
func TestIdleExplicitTracksTheEnvironment(t *testing.T) {
	cases := map[string]struct {
		set          string
		wantTimeout  time.Duration
		wantExplicit bool
	}{
		"unset":       {"", config.DefaultIdleTimeout, false},
		"explicit 0":  {"0", 0, true},
		"explicit 1h": {"1h", time.Hour, true},
		"explicit 5m": {"5m", 5 * time.Minute, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			vars := map[string]string{}
			if tc.set != "" {
				vars[config.EnvIdleTimeout] = tc.set
			}
			c, err := config.LoadFrom(env(vars))
			if err != nil {
				t.Fatalf("LoadFrom: %v", err)
			}
			if c.Idle.Timeout != tc.wantTimeout {
				t.Errorf("Idle.Timeout = %s, want %s", c.Idle.Timeout, tc.wantTimeout)
			}
			if c.Idle.Explicit != tc.wantExplicit {
				t.Errorf("Idle.Explicit = %v, want %v", c.Idle.Explicit, tc.wantExplicit)
			}
		})
	}
}

func TestLaunchdSocketIsRendered(t *testing.T) {
	c := config.Default()
	if s := c.String(); !strings.Contains(s, "launchd.socket=Listener") {
		t.Errorf("String() = %s, want it to name the launchd socket", s)
	}
}

// env builds a getenv over a map.
func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}
