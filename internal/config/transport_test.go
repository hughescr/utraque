package config_test

import (
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/codex/wire"
	"github.com/hughescr/utraque/internal/config"
)

// TestDefaultCodexBaseURLMatchesWire pins config's copy of the Codex backend
// root to the one home in internal/codex/wire. config deliberately imports no
// internal package, so the value is restated there and this test is what
// keeps the two in step.
func TestDefaultCodexBaseURLMatchesWire(t *testing.T) {
	if config.DefaultCodexBaseURL != wire.DefaultBaseURL {
		t.Errorf("config.DefaultCodexBaseURL = %q, want wire.DefaultBaseURL %q", config.DefaultCodexBaseURL, wire.DefaultBaseURL)
	}
}

// envOnly builds a getenv that answers only the given keys, so a test never
// reads the developer's real environment.
func envOnly(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestCodexTransportDefaultsToAuto(t *testing.T) {
	if got := config.Default().Codex.Transport; got != config.TransportModeAuto {
		t.Errorf("Default().Codex.Transport = %q, want %q", got, config.TransportModeAuto)
	}
	// The default must stay auto, not utls: the std transport is what the proxy
	// was live-verified on, and uTLS is a strictly larger attack surface bought
	// for a gate that has never been observed.
	cfg, err := config.LoadFrom(envOnly(nil))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Codex.Transport != config.TransportModeAuto {
		t.Errorf("loaded Codex.Transport = %q, want %q", cfg.Codex.Transport, config.TransportModeAuto)
	}
	if config.TransportModeAuto == config.TransportModeUTLS {
		t.Fatal("auto and utls must be distinct modes")
	}
}

func TestCodexTransportEnvOverride(t *testing.T) {
	cases := map[string]string{
		"std":    config.TransportModeStd,
		"utls":   config.TransportModeUTLS,
		"auto":   config.TransportModeAuto,
		" UTLS ": config.TransportModeUTLS,
		"Std":    config.TransportModeStd,
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			cfg, err := config.LoadFrom(envOnly(map[string]string{config.EnvCodexTransport: raw}))
			if err != nil {
				t.Fatalf("LoadFrom(%q): %v", raw, err)
			}
			if cfg.Codex.Transport != want {
				t.Errorf("Codex.Transport = %q, want %q", cfg.Codex.Transport, want)
			}
		})
	}
}

func TestCodexTransportRejectsUnknownValue(t *testing.T) {
	// Silently defaulting a typo would be indistinguishable from a working
	// uTLS setting until the gate it was set for actually arrived.
	_, err := config.LoadFrom(envOnly(map[string]string{config.EnvCodexTransport: "chrome"}))
	if err == nil {
		t.Fatal("LoadFrom accepted an unknown transport, want an error")
	}
	for _, want := range []string{config.EnvCodexTransport, "chrome", "auto", "std", "utls"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestCodexTransportIsReported(t *testing.T) {
	cfg, err := config.LoadFrom(envOnly(map[string]string{config.EnvCodexTransport: "utls"}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	// A non-default transport changes how every Codex request looks on the
	// wire, so it has to be visible in the startup line and the log record.
	if got := cfg.String(); !strings.Contains(got, "codex.transport=utls") {
		t.Errorf("String() lacks codex.transport=utls: %s", got)
	}
	if got := cfg.LogValue().String(); !strings.Contains(got, "utls") {
		t.Errorf("LogValue() lacks the transport: %s", got)
	}
}

// TestTransportConstantsMatchTheTransportPackage pins the wire values. The
// transport package validates the same enum without config importing it (and
// vice versa), so both sides pin their strings to literals.
func TestTransportConstantsMatchTheTransportPackage(t *testing.T) {
	for got, want := range map[string]string{
		config.TransportModeStd:  "std",
		config.TransportModeUTLS: "utls",
		config.TransportModeAuto: "auto",
	} {
		if got != want {
			t.Errorf("transport constant = %q, want %q", got, want)
		}
	}
}
