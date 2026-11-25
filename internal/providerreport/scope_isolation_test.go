package providerreport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/leg"
	"github.com/hughescr/utraque/internal/providerquota"
	"github.com/hughescr/utraque/internal/usagehistory"
)

// bearerQuotaFixture is a Handler whose Anthropic reader answers a distinct,
// token-derived quota (so a response can be attributed to the bearer whose
// quota it carries) and records every token it was asked to read.
type bearerQuotaFixture struct {
	h *Handler

	mu    sync.Mutex
	calls []string
}

// percentFor maps each test bearer to a unique used_percent.
var percentFor = map[string]float64{"bearer-alpha-canary": 11, "bearer-bravo-canary": 22}

func newBearerQuotaFixture(t *testing.T) *bearerQuotaFixture {
	t.Helper()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	f := &bearerQuotaFixture{}
	f.h = New(Options{
		History: historyFunc(func(context.Context, time.Time, time.Time) (usagehistory.Report, error) {
			return sampleHistory(now), nil
		}),
		Anthropic: anthropicFunc(func(_ context.Context, token string) (providerquota.Observation, error) {
			f.mu.Lock()
			f.calls = append(f.calls, token)
			f.mu.Unlock()
			return providerquota.Observation{
				Source:      leg.Anthropic,
				CollectedAt: now,
				Quotas: []providerquota.Quota{{
					ID: "five_hour", Bucket: "five_hour", Kind: "five_hour",
					UsedPercent: percentFor[token], Unit: providerquota.PercentUnit,
				}},
			}, nil
		}),
		CacheTTL: time.Hour, // everything below happens well inside the TTL
		Now:      func() time.Time { return now },
	})
	return f
}

func (f *bearerQuotaFixture) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// get issues a loopback request, optionally with a bearer, and returns the
// Anthropic section of the decoded report.
func (f *bearerQuotaFixture) get(t *testing.T, bearerToken string) *ProviderReport {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/utraque/providers", nil)
	r.RemoteAddr = "127.0.0.1:4000"
	if bearerToken != "" {
		r.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	for _, tok := range []string{"bearer-alpha-canary", "bearer-bravo-canary"} {
		if strings.Contains(w.Body.String(), tok) {
			t.Fatalf("response leaked bearer %q", tok)
		}
	}
	rep := decodeReport(t, w)
	p := providerNamed(rep, leg.Anthropic)
	if p == nil {
		t.Fatal("report has no anthropic section")
	}
	return p
}

func anthropicPercent(t *testing.T, p *ProviderReport) float64 {
	t.Helper()
	if p.Quota == nil || len(p.Quota.Quotas) != 1 {
		t.Fatalf("anthropic section carries no quota: %+v", p)
	}
	return p.Quota.Quotas[0].UsedPercent
}

// TestProviderCacheIsScopedPerAnthropicBearer: two callers with different
// bearers inside the cache TTL must each trigger their own Anthropic read and
// each see only their own quota.
func TestProviderCacheIsScopedPerAnthropicBearer(t *testing.T) {
	f := newBearerQuotaFixture(t)

	if got := anthropicPercent(t, f.get(t, "bearer-alpha-canary")); got != 11 {
		t.Fatalf("alpha quota=%v want 11", got)
	}
	if got := anthropicPercent(t, f.get(t, "bearer-bravo-canary")); got != 22 {
		t.Fatalf("bravo received quota %v, want its own 22 (alpha's 11 would mean a shared cache entry)", got)
	}
	if n := f.callCount(); n != 2 {
		t.Fatalf("anthropic reader calls=%d want 2 (one per distinct bearer)", n)
	}

	// Same bearer again is served from its own cache entry: no new read, and
	// still its own quota.
	if got := anthropicPercent(t, f.get(t, "bearer-alpha-canary")); got != 11 {
		t.Fatalf("alpha repeat quota=%v want 11", got)
	}
	if n := f.callCount(); n != 2 {
		t.Fatalf("anthropic reader calls=%d want 2 after a cached repeat", n)
	}
}

// TestProviderCacheNeverServesBearerResultToBearerlessCaller covers both
// orders: a bearerless caller must neither receive a bearer's cached quota nor
// poison the entry a later bearer is served from.
func TestProviderCacheNeverServesBearerResultToBearerlessCaller(t *testing.T) {
	for _, bearerFirst := range []bool{true, false} {
		name := "bearer_then_none"
		if !bearerFirst {
			name = "none_then_bearer"
		}
		t.Run(name, func(t *testing.T) {
			f := newBearerQuotaFixture(t)
			var bearerSection, noneSection *ProviderReport
			if bearerFirst {
				bearerSection = f.get(t, "bearer-alpha-canary")
				noneSection = f.get(t, "")
			} else {
				noneSection = f.get(t, "")
				bearerSection = f.get(t, "bearer-alpha-canary")
			}
			if got := anthropicPercent(t, bearerSection); got != 11 {
				t.Fatalf("bearer caller quota=%v want 11", got)
			}
			if noneSection.Quota != nil {
				t.Fatalf("bearerless caller received a quota: %+v", noneSection.Quota)
			}
			if len(noneSection.Errors) == 0 {
				t.Fatal("bearerless caller should see an anthropic credential error, not silence")
			}
			if n := f.callCount(); n != 1 {
				t.Fatalf("anthropic reader calls=%d want 1 (the bearerless caller must not trigger a read)", n)
			}
		})
	}
}
