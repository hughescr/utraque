package providerreport

import (
	"time"

	"github.com/hughescr/utraque/internal/leg"
	"github.com/hughescr/utraque/internal/providerquota"
	"github.com/hughescr/utraque/internal/usagehistory"
)

// ReportV2 is the schema-2 document, served at /utraque/providers/v2. It is
// projected from the same cached Report (schema 1) by renderV2, so the two
// documents always describe one collection. The differences from schema 1,
// each keyed to the finding that motivated it, are:
//
//   - every "source" key is renamed to say what it holds (history.collector,
//     models[].log_source, reference_prices.catalog,
//     configured_plan.provenance) and quota.source, which duplicated
//     providers[].provider, is dropped;
//   - quota_after is quota, the matching errors[].section value is "quota",
//     and quota_before / paired_measurement, which the collector never
//     produced, are gone — calibration.unavailable_reason already says why
//     no bracketed measurement exists;
//   - quota.balances[] holds only funds on hand ({kind, remaining, parts}),
//     the ceilings that were fanned out into balances[]/quotas[]/
//     spend_controls[]/extra_usage are one quota.spend_limits[] list, and
//     scope_id is limit_id;
//   - quota.quotas[].id follows one rule for every provider,
//     bucket[:slot][:scope], the bucket is exposed, and kind is utraque's
//     closed WindowKind vocabulary;
//   - last_success and last_attempt are cross-attempt memory, and
//     last_complete_snapshot carries measured_at;
//   - conditional_remaining_token_estimates[].balance is remaining.
type ReportV2 struct {
	SchemaVersion       int                `json:"schema_version"`
	GeneratedAt         time.Time          `json:"generated_at"`
	CollectionStartedAt time.Time          `json:"collection_started_at"`
	CollectionEndedAt   time.Time          `json:"collection_ended_at"`
	HistoryRange        DateRange          `json:"history_range"`
	Providers           []ProviderReportV2 `json:"providers"`
	UnattributedHistory []ModelStatsV2     `json:"unattributed_history,omitempty"`
}

// ProviderReportV2 is one leg's schema-2 section.
type ProviderReportV2 struct {
	Provider leg.ID `json:"provider"`
	Status   Status `json:"status"`
	// LastAttempt is when the most recent collection attempt for this
	// provider ended, remembered across attempts for as long as the cache
	// entry lives (providerMemory). Unlike schema 1 it is never moved to the
	// quota leg's real upstream attempt: that instant stays on the quota
	// error as errors[].attempted_at.
	LastAttempt time.Time `json:"last_attempt"`
	// LastSuccess is the end of the most recent attempt in which at least
	// one section (quota or history) succeeded, remembered across attempts:
	// a failed attempt keeps the previous value. Omitted until one attempt
	// has succeeded.
	LastSuccess     *time.Time            `json:"last_success,omitempty"`
	SourceFreshness Freshness             `json:"source_freshness"`
	Errors          []ReportError         `json:"errors"`
	Quota           *QuotaReadingV2       `json:"quota,omitempty"`
	History         *HistorySummaryV2     `json:"history,omitempty"`
	ReferencePrices *PriceSnapshotV2      `json:"reference_prices,omitempty"`
	Calibration     *Calibration          `json:"calibration,omitempty"`
	Remaining       []RemainingEstimateV2 `json:"conditional_remaining_token_estimates,omitempty"`
	ConfiguredPlan  *ConfiguredPlanV2     `json:"configured_plan,omitempty"`
	LastComplete    *ProviderSnapshotV2   `json:"last_complete_snapshot,omitempty"`
}

// ProviderSnapshotV2 is the schema-2 form of ProviderSnapshot: the retained
// complete measurement, stamped with when it was taken.
type ProviderSnapshotV2 struct {
	// MeasuredAt is the end of the attempt in which every section of this
	// snapshot succeeded together (schema 1's last_complete_snapshot
	// .last_success).
	MeasuredAt  *time.Time            `json:"measured_at,omitempty"`
	Freshness   Freshness             `json:"source_freshness"`
	Quota       *QuotaReadingV2       `json:"quota,omitempty"`
	History     *HistorySummaryV2     `json:"history,omitempty"`
	Calibration *Calibration          `json:"calibration,omitempty"`
	Remaining   []RemainingEstimateV2 `json:"conditional_remaining_token_estimates,omitempty"`
}

// QuotaReadingV2 is the schema-2 view of a providerquota.Observation. The
// leg it belongs to is the enclosing providers[].provider.
type QuotaReadingV2 struct {
	CollectedAt time.Time `json:"collected_at"`
	// Quotas holds the rolling usage windows: Anthropic's limits and Codex's
	// primary/secondary windows. A Codex bucket's spend ceiling is not a
	// window and lives in SpendLimits.
	Quotas []QuotaV2 `json:"quotas,omitempty"`
	// Balances holds funds on hand only: DeepSeek's account balance and Codex
	// workspace credits.
	Balances []BalanceV2 `json:"balances,omitempty"`
	// SpendLimits holds every spending ceiling with consumption against it:
	// one per Codex bucket that reports one, and Anthropic's extra usage.
	SpendLimits  []providerquota.SpendLimit  `json:"spend_limits,omitempty"`
	Plan         *providerquota.PlanInfo     `json:"plan,omitempty"`
	ResetCredits *providerquota.ResetCredits `json:"reset_credits,omitempty"`
	Available    *bool                       `json:"available,omitempty"`
}

// QuotaV2 is one rolling usage window at schema 2. The provider-specific
// decorations keep their schema-1 meaning and are documented on
// providerquota.Quota.
type QuotaV2 struct {
	// ID is bucket[:slot][:scope] (providerquota.QuotaID) and is unique
	// within the reading for every provider.
	ID string `json:"id"`
	// Bucket is the upstream identity the ID is derived from: Codex's
	// limitId, Anthropic's limit kind (or legacy window key).
	Bucket string `json:"bucket"`
	// Kind is utraque's closed vocabulary (providerquota.WindowKindOf).
	Kind providerquota.WindowKind `json:"kind"`
	// Name is Codex's limitName.
	Name string `json:"name,omitempty"`
	// Group is Anthropic's upstream limit group.
	Group string `json:"group,omitempty"`
	// Slot is Codex's window position, "primary" or "secondary".
	Slot            string                  `json:"slot,omitempty"`
	UsedPercent     float64                 `json:"used_percent"`
	Unit            string                  `json:"unit"`
	DurationSeconds *int64                  `json:"duration_seconds,omitempty"`
	ResetsAt        *time.Time              `json:"resets_at,omitempty"`
	Scope           *providerquota.Scope    `json:"scope,omitempty"`
	Active          *bool                   `json:"active,omitempty"`
	Plan            *providerquota.PlanInfo `json:"plan,omitempty"`
	ReachedType     string                  `json:"reached_type,omitempty"`
}

// BalanceV2 is funds on hand at schema 2: Remaining is always spendable and
// Parts, when present, sum to it.
type BalanceV2 struct {
	Kind providerquota.BalanceKind `json:"kind"`
	// LimitID is the Codex bucket the credits belong to (the paired
	// quotas[].bucket); empty for DeepSeek's account-wide balance.
	LimitID    string `json:"limit_id,omitempty"`
	Currency   string `json:"currency,omitempty"`
	AmountUnit string `json:"amount_unit,omitempty"`
	// Remaining is the schema-1 "total".
	Remaining string `json:"remaining,omitempty"`
	// Parts is the schema-1 "components": DeepSeek's granted and topped_up
	// portions of Remaining.
	Parts     []providerquota.BalanceComponent `json:"parts,omitempty"`
	Available *bool                            `json:"available,omitempty"`
	Unlimited *bool                            `json:"unlimited,omitempty"`
}

// HistorySummaryV2 is HistorySummary with "source" named for what it is.
type HistorySummaryV2 struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Collector is the tool that produced the history ("ccusage").
	Collector                  string                      `json:"collector"`
	Coverage                   string                      `json:"coverage"`
	CostBasis                  string                      `json:"cost_basis"`
	ToolVersion                string                      `json:"tool_version,omitempty"`
	InvocationMode             usagehistory.InvocationMode `json:"invocation_mode"`
	UnitPricesAvailable        bool                        `json:"unit_prices_available"`
	UnitPriceUnavailableReason string                      `json:"unit_price_unavailable_reason"`
	SevenDays                  PeriodSummaryV2             `json:"seven_days"`
	ThirtyDays                 PeriodSummaryV2             `json:"thirty_days"`
	// Blocks are usagehistory's own rows, unchanged from schema 1; their
	// "source" is the ccusage agent label and is always "claude".
	Blocks []usagehistory.BlockSummary `json:"blocks"`
	Issues []usagehistory.Issue        `json:"issues"`
}

// PeriodSummaryV2 is PeriodSummary over ModelStatsV2 rows.
type PeriodSummaryV2 struct {
	Since  time.Time      `json:"since"`
	Until  time.Time      `json:"until"`
	Models []ModelStatsV2 `json:"models"`
}

// ModelStatsV2 is ModelStats with "source" named for what it is.
type ModelStatsV2 struct {
	// LogSource is the ccusage agent label of the local log the rows came
	// from ("claude", "codex", "opencode"); it never names a provider.
	LogSource                   string                  `json:"log_source"`
	Model                       string                  `json:"model"`
	Provider                    leg.ID                  `json:"provider"`
	InputTokens                 uint64                  `json:"input_tokens"`
	OutputTokens                uint64                  `json:"output_tokens"`
	CacheCreationTokens         uint64                  `json:"cache_creation_tokens"`
	CacheReadTokens             uint64                  `json:"cache_read_tokens"`
	TotalTokens                 uint64                  `json:"total_tokens"`
	CostUSD                     *float64                `json:"cost_usd"`
	CostStatus                  usagehistory.CostStatus `json:"cost_status"`
	HistoricalEffectiveUSDToken *float64                `json:"historical_effective_usd_per_token"`
	UnitPriceUnavailableReason  string                  `json:"unit_price_unavailable_reason,omitempty"`
}

// PriceSnapshotV2 is PriceSnapshot with "source" named for what it is.
type PriceSnapshotV2 struct {
	// Catalog is the public price catalog the rows came from ("models.dev").
	Catalog     string     `json:"catalog"`
	ObservedAt  time.Time  `json:"observed_at"`
	Stale       bool       `json:"stale,omitempty"`
	Unit        string     `json:"unit"`
	Models      []PriceRow `json:"models"`
	Assumptions []string   `json:"assumptions,omitempty"`
}

// RemainingEstimateV2 is RemainingEstimate with the divided figure under the
// same key the balances[] row uses.
type RemainingEstimateV2 struct {
	Model    string `json:"model"`
	Currency string `json:"currency"`
	// Remaining is the balances[] row's remaining funds the estimate divides.
	Remaining          string            `json:"remaining"`
	Tokens             *float64          `json:"tokens,omitempty"`
	HistoricalUSDToken *float64          `json:"historical_effective_usd_per_token,omitempty"`
	Assumptions        []string          `json:"assumptions,omitempty"`
	UnavailableReason  UnavailableReason `json:"unavailable_reason,omitempty"`
}

// ConfiguredPlanV2 is ConfiguredPlan with "source" named for what it is.
type ConfiguredPlanV2 struct {
	Label      string   `json:"label,omitempty"`
	Multiplier *float64 `json:"multiplier,omitempty"`
	// Provenance is always "configured": the plan is operator-supplied
	// metadata, never provider-confirmed.
	Provenance string `json:"provenance"`
}

// renderV2 projects a schema-1 document, as the collector built it and
// markFreshness post-processed it, into schema 2. memory supplies the
// cross-attempt last_attempt / last_success per provider; a provider absent
// from memory (which storeAttempt never leaves) falls back to the attempt's
// own values.
func renderV2(r Report, memory map[leg.ID]providerMemory) ReportV2 {
	out := ReportV2{SchemaVersion: SchemaVersionV2, GeneratedAt: r.GeneratedAt,
		CollectionStartedAt: r.CollectionStartedAt, CollectionEndedAt: r.CollectionEndedAt,
		HistoryRange: r.HistoryRange, Providers: make([]ProviderReportV2, 0, len(r.Providers)),
		UnattributedHistory: modelStatsV2(r.UnattributedHistory)}
	for _, p := range r.Providers {
		out.Providers = append(out.Providers, renderProviderV2(p, memory[p.Provider]))
	}
	return out
}

func renderProviderV2(p ProviderReport, mem providerMemory) ProviderReportV2 {
	out := ProviderReportV2{Provider: p.Provider, Status: p.Status,
		LastAttempt: p.LastAttempt, LastSuccess: p.LastSuccess,
		SourceFreshness: p.SourceFreshness, Errors: make([]ReportError, 0, len(p.Errors)),
		Quota: quotaReadingV2(p.Provider, p.Quota), History: historyV2(p.History),
		Calibration: p.Calibration, Remaining: remainingV2(p.Remaining)}
	if !mem.lastAttemptAt.IsZero() {
		out.LastAttempt = mem.lastAttemptAt
	}
	if !mem.lastSuccessAt.IsZero() {
		t := mem.lastSuccessAt
		out.LastSuccess = &t
	}
	for _, e := range p.Errors {
		if e.Section == SectionQuota {
			e.Section = SectionQuotaV2
		}
		out.Errors = append(out.Errors, e)
	}
	if p.ReferencePrices != nil {
		out.ReferencePrices = &PriceSnapshotV2{Catalog: p.ReferencePrices.Source, ObservedAt: p.ReferencePrices.ObservedAt,
			Stale: p.ReferencePrices.Stale, Unit: p.ReferencePrices.Unit, Models: p.ReferencePrices.Models,
			Assumptions: p.ReferencePrices.Assumptions}
	}
	if p.ConfiguredPlan != nil {
		out.ConfiguredPlan = &ConfiguredPlanV2{Label: p.ConfiguredPlan.Label, Multiplier: p.ConfiguredPlan.Multiplier,
			Provenance: p.ConfiguredPlan.Source}
	}
	if s := p.LastComplete; s != nil {
		out.LastComplete = &ProviderSnapshotV2{MeasuredAt: s.LastSuccess, Freshness: s.Freshness,
			Quota: quotaReadingV2(p.Provider, s.Quota), History: historyV2(s.History),
			Calibration: s.Calibration, Remaining: remainingV2(s.Remaining)}
	}
	return out
}

func remainingV2(in []RemainingEstimate) []RemainingEstimateV2 {
	if in == nil {
		return nil
	}
	out := make([]RemainingEstimateV2, len(in))
	for i, e := range in {
		out[i] = RemainingEstimateV2{Model: e.Model, Currency: e.Currency, Remaining: e.Balance, Tokens: e.Tokens,
			HistoricalUSDToken: e.HistoricalUSDToken, Assumptions: e.Assumptions, UnavailableReason: e.UnavailableReason}
	}
	return out
}

func quotaReadingV2(provider leg.ID, o *providerquota.Observation) *QuotaReadingV2 {
	if o == nil {
		return nil
	}
	out := &QuotaReadingV2{CollectedAt: o.CollectedAt, Plan: o.Plan, ResetCredits: o.ResetCredits, Available: o.Available}
	for _, q := range o.Quotas {
		if q.Kind == providerquota.QuotaKindSpendControl {
			// A schema-1 fan-out of the bucket's SpendLimit, not a window.
			continue
		}
		if q.Bucket == "" {
			// An observation assembled without going through a provider
			// normaliser (every normaliser sets Bucket): the schema-1 id is
			// the closest thing to a bucket it has.
			q.Bucket = q.ID
		}
		out.Quotas = append(out.Quotas, QuotaV2{ID: providerquota.QuotaID(q.Bucket, q.Slot, q.Scope), Bucket: q.Bucket,
			Kind: providerquota.WindowKindOf(provider, q), Name: q.Name, Group: q.Group, Slot: q.Slot,
			UsedPercent: q.UsedPercent, Unit: q.Unit, DurationSeconds: q.DurationSeconds, ResetsAt: q.ResetsAt,
			Scope: q.Scope, Active: q.Active, Plan: q.Plan, ReachedType: q.ReachedType})
	}
	for _, b := range o.Balances {
		if b.Kind == providerquota.BalanceKindSpendControl {
			// A schema-1 fan-out of the bucket's SpendLimit: its total is a
			// ceiling, not funds on hand.
			continue
		}
		out.Balances = append(out.Balances, BalanceV2{Kind: b.Kind, LimitID: b.LimitID, Currency: b.Currency,
			AmountUnit: b.AmountUnit, Remaining: b.Total, Parts: b.Components, Available: b.Available, Unlimited: b.Unlimited})
	}
	if len(o.SpendLimits) > 0 {
		out.SpendLimits = o.SpendLimits
	}
	return out
}

func historyV2(h *HistorySummary) *HistorySummaryV2 {
	if h == nil {
		return nil
	}
	return &HistorySummaryV2{StartedAt: h.StartedAt, FinishedAt: h.FinishedAt, Collector: h.Source,
		Coverage: h.Coverage, CostBasis: h.CostBasis, ToolVersion: h.ToolVersion, InvocationMode: h.InvocationMode,
		UnitPricesAvailable: h.UnitPricesAvailable, UnitPriceUnavailableReason: h.UnitPriceUnavailableReason,
		SevenDays:  PeriodSummaryV2{Since: h.SevenDays.Since, Until: h.SevenDays.Until, Models: modelStatsV2(h.SevenDays.Models)},
		ThirtyDays: PeriodSummaryV2{Since: h.ThirtyDays.Since, Until: h.ThirtyDays.Until, Models: modelStatsV2(h.ThirtyDays.Models)},
		Blocks:     h.Blocks, Issues: h.Issues}
}

func modelStatsV2(in []ModelStats) []ModelStatsV2 {
	if in == nil {
		return nil
	}
	out := make([]ModelStatsV2, len(in))
	for i, m := range in {
		out[i] = ModelStatsV2{LogSource: m.Source, Model: m.Model, Provider: m.Provider,
			InputTokens: m.InputTokens, OutputTokens: m.OutputTokens, CacheCreationTokens: m.CacheCreationTokens,
			CacheReadTokens: m.CacheReadTokens, TotalTokens: m.TotalTokens, CostUSD: m.CostUSD, CostStatus: m.CostStatus,
			HistoricalEffectiveUSDToken: m.HistoricalEffectiveUSDToken, UnitPriceUnavailableReason: m.UnitPriceUnavailableReason}
	}
	return out
}
