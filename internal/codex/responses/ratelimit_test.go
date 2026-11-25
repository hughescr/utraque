package responses

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRateLimitDurations(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
	}{{"-1", false}, {"9223372036854775807", false}, {"0", true}, {"60", true}} {
		h := make(http.Header)
		h.Set("X-Codex-Primary-Window-Minutes", tc.raw)
		h.Set("X-Codex-Primary-Reset-After-Seconds", tc.raw)
		h.Set("Retry-After", tc.raw)
		rl := ParseRateLimits(h, time.Time{})
		if rl.Primary.HasWindowMinutes != tc.ok || rl.Primary.HasResetAfter != tc.ok || rl.HasRetryAfter != tc.ok {
			t.Errorf("duration fields with %q: want reported=%v", tc.raw, tc.ok)
		}
	}
}

func TestParseRateLimitsUsedPercent(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want float64
		ok   bool
	}{
		{"", 0, false}, {"bad", 0, false}, {"NaN", 0, false},
		{"+Inf", 0, false}, {"-Inf", 0, false}, {"1e999", 0, false},
		{"-0.01", 0, false}, {"100.01", 0, false}, {"1e300", 0, false},
		{"0", 0, true}, {" 42.5 ", 42.5, true}, {"100", 100, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			h := make(http.Header)
			h.Set("X-Codex-Primary-Used-Percent", tc.raw)
			w := ParseRateLimits(h, time.Time{}).Primary
			if w.HasUsedPercent != tc.ok || w.UsedPercent != tc.want {
				t.Errorf("used percent = (%v, %t), want (%v, %t)", w.UsedPercent, w.HasUsedPercent, tc.want, tc.ok)
			}
		})
	}
}
