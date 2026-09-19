package providerquota

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestCloneKeepsEveryFieldAndSharesNothing walks the Codex and Anthropic wire
// fixtures, which populate every field the normalisers can set, and checks
// that Clone is equal to the original (including the fields a JSON round trip
// drops) and that mutating the clone leaves the original untouched.
func TestCloneKeepsEveryFieldAndSharesNothing(t *testing.T) {
	for _, tt := range []struct {
		name string
		obs  Observation
	}{
		{"codex", normalizeCodexBody(t, codexWireFixture)},
		{"anthropic", readAnthropicBody(t, anthropicModernWireFixture)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := tt.obs
			if len(original.SpendLimits) == 0 || original.Quotas[0].Bucket == "" {
				t.Fatal("fixture does not populate the fields JSON drops")
			}
			var viaJSON Observation
			b, _ := json.Marshal(original)
			_ = json.Unmarshal(b, &viaJSON)
			if len(viaJSON.SpendLimits) != 0 || viaJSON.Quotas[0].Bucket != "" {
				t.Fatal("JSON round trip now keeps the internal fields; Clone may be redundant")
			}
			clone := original.Clone()
			if !reflect.DeepEqual(clone, original) {
				t.Fatalf("clone differs\n got: %+v\nwant: %+v", clone, original)
			}
			before, _ := json.Marshal(original)
			clone.Quotas[0].Bucket = "changed"
			clone.Quotas[0].ID = "changed"
			if clone.Quotas[0].ResetsAt != nil {
				*clone.Quotas[0].ResetsAt = clone.Quotas[0].ResetsAt.Add(1)
			}
			if clone.Quotas[0].Active != nil {
				*clone.Quotas[0].Active = !*clone.Quotas[0].Active
			}
			if len(clone.Balances) > 0 {
				clone.Balances[0].Total = "changed"
				if clone.Balances[0].Available != nil {
					*clone.Balances[0].Available = !*clone.Balances[0].Available
				}
			}
			clone.SpendLimits[0].LimitID = "changed"
			if clone.SpendLimits[0].Limit != nil {
				*clone.SpendLimits[0].Limit = "changed"
			}
			if clone.SpendLimits[0].Enabled != nil {
				*clone.SpendLimits[0].Enabled = !*clone.SpendLimits[0].Enabled
			}
			if clone.ExtraUsage != nil && clone.ExtraUsage.MonthlyLimit != nil {
				*clone.ExtraUsage.MonthlyLimit = "changed"
			}
			if clone.ResetCredits != nil {
				clone.ResetCredits.AvailableCount++
			}
			for i := range clone.Quotas {
				if clone.Quotas[i].Scope != nil && clone.Quotas[i].Scope.Model != nil {
					clone.Quotas[i].Scope.Model.ID = "changed"
				}
			}
			after, _ := json.Marshal(original)
			if string(before) != string(after) || original.Quotas[0].Bucket == "changed" || original.SpendLimits[0].LimitID == "changed" {
				t.Fatalf("mutating the clone changed the original:\nbefore: %s\nafter:  %s", before, after)
			}
			if clone.CacheScope() != original.CacheScope() {
				t.Fatal("cache scope not carried")
			}
		})
	}
}

func TestQuotaIDRuleAndWindowKinds(t *testing.T) {
	scope := &Scope{Model: &ScopeLabel{ID: "claude-opus-5"}, Surface: &ScopeLabel{DisplayName: "Code"}}
	if got := QuotaID("weekly_scoped", "", scope); got != "weekly_scoped:model=claude-opus-5:surface=Code" {
		t.Fatalf("anthropic id=%q", got)
	}
	if got := QuotaID("codex", "primary", nil); got != "codex:primary" {
		t.Fatalf("codex id=%q", got)
	}
	if got := QuotaID("codex", "", nil); got != "codex" {
		t.Fatalf("bare id=%q", got)
	}
	// Anthropic classification by kind, then by group with scope; legacy ids.
	for _, tc := range []struct {
		q    Quota
		want WindowKind
	}{
		{Quota{Kind: "session"}, WindowSession},
		{Quota{Kind: "weekly_all"}, WindowWeekly},
		{Quota{Kind: "weekly_all", Scope: scope}, WindowWeeklyScoped},
		{Quota{Kind: "weekly_scoped", Scope: scope}, WindowWeeklyScoped},
		{Quota{Kind: "weekly_scoped"}, WindowWeeklyScoped},
		{Quota{Kind: "five_hour"}, WindowSession},
		{Quota{Kind: "seven_day"}, WindowWeekly},
		{Quota{Kind: "seven_day_opus"}, WindowWeeklyScoped},
		{Quota{Kind: "seven_day_sonnet"}, WindowWeeklyScoped},
		{Quota{Kind: "seven_day_oauth_apps"}, WindowWeeklyScoped},
		{Quota{Kind: "cinder_cove"}, WindowOther},
		{Quota{Kind: "novel", Group: "session"}, WindowSession},
		{Quota{Kind: "novel", Group: "weekly"}, WindowWeekly},
		{Quota{Kind: "novel", Group: "weekly", Scope: scope}, WindowWeeklyScoped},
		{Quota{Kind: "novel", Group: "novel"}, WindowOther},
		{Quota{Kind: "novel", Group: "novel", Scope: scope}, WindowOther},
		{Quota{Kind: "session", Scope: scope}, WindowSession},
		{Quota{Bucket: "five_hour"}, WindowSession},
	} {
		if got := WindowKindOf("anthropic", tc.q); got != tc.want {
			t.Errorf("anthropic %+v -> %q want %q", tc.q, got, tc.want)
		}
	}
	for _, tc := range []struct {
		q    Quota
		want WindowKind
	}{
		{Quota{Slot: "primary"}, WindowSession},
		{Quota{Slot: "secondary"}, WindowWeekly},
		{Quota{Kind: QuotaKindSpendControl}, WindowOther},
	} {
		if got := WindowKindOf("codex", tc.q); got != tc.want {
			t.Errorf("codex %+v -> %q want %q", tc.q, got, tc.want)
		}
	}
	if got := WindowKindOf("deepseek", Quota{Slot: "primary"}); got != WindowOther {
		t.Errorf("deepseek -> %q", got)
	}
}
