package providerquota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/leg"
)

// The expected documents below were generated from the tree at commit 7f2c11b
// (before Quota.Bucket, QuotaKind and Observation.SpendLimits existed) by
// marshalling the same fixtures. They pin the schema v1 wire shape: the
// internal-only fields must not appear and nothing else may move.

const codexWireFixture = `{"accountId":"account-private-id","rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":"Codex","planType":"plus","primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":1789135200},"secondary":{"usedPercent":40,"windowDurationMins":10080,"resetsAt":1789732800},"credits":{"balance":"12.50","hasCredits":true,"unlimited":false},"individualLimit":{"limit":"50.00","used":"12.25","remainingPercent":75.5,"resetsAt":1789732800},"rateLimitReachedType":"none","spendControlReached":false},"codex_other":{"limitId":"codex_other","primary":{"usedPercent":0,"windowDurationMins":60,"resetsAt":1789135200},"spendControlReached":true},"codex_bare":{"individualLimit":{"limit":"10","used":"10","remainingPercent":0,"resetsAt":1789135200}}},"rateLimitResetCredits":{"availableCount":2}}`

const codexWireExpected = `{"source":"codex","collected_at":"2026-09-11T12:00:00Z","quotas":[{"id":"codex","name":"Codex","slot":"primary","used_percent":25,"unit":"percent_0_100","duration_seconds":18000,"resets_at":"2026-09-11T14:00:00Z","plan":{"type":"plus"},"reached_type":"none"},{"id":"codex","name":"Codex","slot":"secondary","used_percent":40,"unit":"percent_0_100","duration_seconds":604800,"resets_at":"2026-09-18T12:00:00Z","plan":{"type":"plus"},"reached_type":"none"},{"id":"codex:spend_control","kind":"spend_control","used_percent":24.5,"unit":"percent_0_100","resets_at":"2026-09-18T12:00:00Z"},{"id":"codex_bare:spend_control","kind":"spend_control","used_percent":100,"unit":"percent_0_100","resets_at":"2026-09-11T14:00:00Z"},{"id":"codex_other","slot":"primary","used_percent":0,"unit":"percent_0_100","duration_seconds":3600,"resets_at":"2026-09-11T14:00:00Z"}],"balances":[{"kind":"workspace_credits","scope_id":"codex","amount_unit":"credits","total":"12.50","available":true,"unlimited":false},{"kind":"spend_control","scope_id":"codex","amount_unit":"provider_units","total":"50.00","components":[{"name":"used","amount":"12.25"}]},{"kind":"spend_control","scope_id":"codex_bare","amount_unit":"provider_units","total":"10","components":[{"name":"used","amount":"10"}]}],"spend_controls":[{"scope_id":"codex","reached":false},{"scope_id":"codex_other","reached":true}],"reset_credits":{"available_count":2}}`

const anthropicModernWireFixture = `{
	"five_hour":{"utilization":99,"resets_at":"2026-09-11T14:00:00Z"},
	"limits":[
		{"kind":"session","group":"session","percent":4,"resets_at":"2026-09-11T14:00:00Z","scope":null,"is_active":true},
		{"kind":"weekly_scoped","group":"weekly","percent":16,"resets_at":null,"scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false},
		{"kind":"weekly_all","group":"weekly","percent":50,"resets_at":"2026-09-18T00:00:00Z","scope":{"model":{"id":"claude-opus-5","display_name":"Opus"},"surface":{"id":"code","display_name":"Code"}},"is_active":true}
	],
	"extra_usage":{"is_enabled":true,"monthly_limit":100,"used_credits":25.5,"utilization":25.5,"currency":"USD"}
}`

const anthropicModernWireExpected = `{"source":"anthropic","collected_at":"2026-09-11T12:00:00Z","quotas":[{"id":"session","kind":"session","group":"session","used_percent":4,"unit":"percent_0_100","duration_seconds":18000,"resets_at":"2026-09-11T14:00:00Z","active":true},{"id":"weekly_scoped:model=Fable","kind":"weekly_scoped","group":"weekly","used_percent":16,"unit":"percent_0_100","duration_seconds":604800,"scope":{"model":{"display_name":"Fable"}},"active":false},{"id":"weekly_all:model=claude-opus-5:surface=code","kind":"weekly_all","group":"weekly","used_percent":50,"unit":"percent_0_100","duration_seconds":604800,"resets_at":"2026-09-18T00:00:00Z","scope":{"model":{"id":"claude-opus-5","display_name":"Opus"},"surface":{"id":"code","display_name":"Code"}},"active":true}],"extra_usage":{"enabled":true,"monthly_limit":"100","used_credits":"25.5","amount_unit":"provider_units","used_percent":25.5,"unit":"percent_0_100","currency":"USD"}}`

const anthropicLegacyWireFixture = `{"five_hour":{"utilization":0,"resets_at":"2026-09-11T14:00:00Z"},"seven_day":{"utilization":12,"resets_at":"2026-09-18T00:00:00Z"},"cinder_cove":{"utilization":3},"extra_usage":{"is_enabled":false}}`

const anthropicLegacyWireExpected = `{"source":"anthropic","collected_at":"2026-09-11T12:00:00Z","quotas":[{"id":"five_hour","kind":"five_hour","used_percent":0,"unit":"percent_0_100","duration_seconds":18000,"resets_at":"2026-09-11T14:00:00Z"},{"id":"seven_day","kind":"seven_day","used_percent":12,"unit":"percent_0_100","duration_seconds":604800,"resets_at":"2026-09-18T00:00:00Z"},{"id":"cinder_cove","kind":"cinder_cove","used_percent":3,"unit":"percent_0_100"}],"extra_usage":{"enabled":false}}`

// readAnthropicBody serves body from a local server and reads it as Anthropic
// usage with the fixed clock.
func readAnthropicBody(t *testing.T, body string) Observation {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer server.Close()
	client, err := NewAnthropicClient(AnthropicOptions{URL: server.URL, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Read(context.Background(), "token")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// normalizeCodexBody decodes a rateLimits/read result and normalizes it as a
// Codex observation with the fixed clock.
func normalizeCodexBody(t *testing.T, body string) Observation {
	t.Helper()
	var response codexRateLimitsResponse
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	o := Observation{Source: leg.Codex, CollectedAt: fixedNow}
	if err := normalizeCodexLimits(&o, response, fixedNow); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestObservationWireUnchanged(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  Observation
		want string
	}{
		{"codex", normalizeCodexBody(t, codexWireFixture), codexWireExpected},
		{"anthropic modern", readAnthropicBody(t, anthropicModernWireFixture), anthropicModernWireExpected},
		{"anthropic legacy", readAnthropicBody(t, anthropicLegacyWireFixture), anthropicLegacyWireExpected},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.got.SpendLimits) == 0 {
				t.Fatal("fixture does not exercise SpendLimits")
			}
			wire, err := json.Marshal(tt.got)
			if err != nil {
				t.Fatal(err)
			}
			if string(wire) != tt.want {
				t.Errorf("wire changed\n got: %s\nwant: %s", wire, tt.want)
			}
		})
	}
}
